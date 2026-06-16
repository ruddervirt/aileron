'use strict';

// Clients connecting before a VM's console exists must be HELD (keepalive
// nops, no guacd churn) and attached the moment the console probes
// reachable — connect once, no client-side retry loop.

const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const net = require('net');
const WebSocket = require('ws');

const { buildConfig, createGateway } = require('../server');

function enc(...elems) {
  return elems.map((e) => `${[...e].length}.${e}`).join(',') + ';';
}

function waitFor(predicate, what, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    (function poll() {
      if (predicate()) {
        return resolve();
      }
      if (Date.now() > deadline) {
        return reject(new Error(`timed out waiting for ${what}`));
      }
      setTimeout(poll, 25);
    })();
  });
}

function startFakeGuacd() {
  const state = { conns: 0 };
  state.server = net.createServer((sock) => {
    state.conns++;
    let buf = '';
    sock.on('data', (d) => {
      buf += d.toString('utf8');
      if (buf.includes('select') && !sock._args) {
        sock._args = true;
        sock.write(enc('args', 'VERSION_1_1_0', 'hostname', 'port'));
      }
      if (buf.includes('connect') && !sock._ready) {
        sock._ready = true;
        sock.write(enc('ready', `$cw-${state.conns}`));
        sock.write(enc('size', '0', '800', '600'));
        sock.write(enc('sync', '1'));
      }
    });
    sock.on('error', () => {});
  });
  return new Promise((resolve) => {
    state.server.listen(0, '127.0.0.1', () => resolve(state));
  });
}

// Bridge stub whose /probe answer is controlled by state.consoleUp.
function startBridgeStub(state) {
  const srv = http.createServer((req, res) => {
    if (req.url === '/probe') {
      req.resume();
      res.statusCode = state.consoleUp ? 204 : 503;
      res.end();
      return;
    }
    req.resume();
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ port: 59004 }));
  });
  return new Promise((resolve) => {
    srv.listen(0, '127.0.0.1', () => resolve(srv));
  });
}

function openClient(url) {
  const ws = new WebSocket(url);
  const c = { ws, closed: false, nops: 0, display: false, error: null };
  ws.on('close', () => {
    c.closed = true;
  });
  let buf = '';
  ws.on('message', (d) => {
    buf += d.toString();
    if (buf.includes('3.nop;')) {
      c.nops++;
      buf = buf.replace(/3\.nop;/g, '');
    }
    if (/\d+\.(size|img|blob)/.test(buf)) {
      c.display = true;
    }
    const m = buf.match(/5\.error,\d+\.[^,]+,\d+\.(\d+);/);
    if (m) {
      c.error = m[1];
    }
    if (buf.length > 65536) {
      buf = '';
    }
  });
  return new Promise((resolve, reject) => {
    ws.on('open', () => resolve(c));
    ws.on('error', reject);
  });
}

test('client is held until console appears, then attaches', async (t) => {
  const guacd = await startFakeGuacd();
  t.after(() => guacd.server.close());
  const probeState = { consoleUp: false };
  const bridgeStub = await startBridgeStub(probeState);
  t.after(() => bridgeStub.close());

  const config = buildConfig({
    GUACD_PORT: String(guacd.server.address().port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    GUAC_LOG_LEVEL: 'QUIET',
    PROBE_INTERVAL_MS: '100',
  });
  const { server, gl, registry } = createGateway(config);
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  t.after(() => {
    gl.close();
    server.close();
  });

  const c = await openClient(
    `ws://127.0.0.1:${server.address().port}/internal/ns/cw-vmi`
  );

  // Console down: held open with nops, no guacd connections, no session.
  await waitFor(() => c.nops >= 3, 'keepalive nops');
  assert.strictEqual(c.closed, false, 'connection must stay open while waiting');
  assert.strictEqual(guacd.conns, 0, 'no guacd churn while console is down');
  assert.strictEqual(registry.size(), 0);

  // Console appears: client attaches without reconnecting.
  probeState.consoleUp = true;
  await waitFor(() => c.display, 'display data after console came up');
  assert.strictEqual(c.closed, false);
  assert.strictEqual(guacd.conns, 1, 'exactly one guacd connection');
  assert.strictEqual(registry.size(), 1);
  c.ws.close();
});

test('console wait times out with a clean error', async (t) => {
  const guacd = await startFakeGuacd();
  t.after(() => guacd.server.close());
  const bridgeStub = await startBridgeStub({ consoleUp: false });
  t.after(() => bridgeStub.close());

  const config = buildConfig({
    GUACD_PORT: String(guacd.server.address().port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    GUAC_LOG_LEVEL: 'QUIET',
    PROBE_INTERVAL_MS: '50',
    CONSOLE_WAIT_MS: '300',
  });
  const { server, gl } = createGateway(config);
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  t.after(() => {
    gl.close();
    server.close();
  });

  const c = await openClient(
    `ws://127.0.0.1:${server.address().port}/internal/ns/cw-timeout`
  );
  await waitFor(() => c.closed, 'timeout close');
  assert.strictEqual(c.error, '519', 'client must receive a clean error instruction');
  assert.strictEqual(guacd.conns, 0);
});
