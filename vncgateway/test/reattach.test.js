'use strict';

// Transparent re-attach: when guacd kills the connection (QEMU drops VNC
// clients during boot-time display re-inits), a browser's websocket must NOT
// close — the gateway re-attaches the same socket to a fresh session and the
// viewer gets a new full frame.

const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const net = require('net');
const WebSocket = require('ws');

const { buildConfig, createGateway } = require('../server');

function enc(...elems) {
  return elems.map((e) => `${[...e].length}.${e}`).join(',') + ';';
}

function waitFor(predicate, what, timeoutMs = 8000) {
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

test('viewer websocket survives guacd death via re-attach', async (t) => {
  // Fake guacd: serves connections, killConnection() sends an error
  // instruction to live sockets without closing them (observed guacd
  // behavior).
  const state = { conns: [] };
  const guacd = net.createServer((sock) => {
    const conn = { sock, dead: false, id: null };
    state.conns.push(conn);
    let buf = '';
    sock.on('data', (d) => {
      buf += d.toString('utf8');
      if (buf.includes('select') && !conn._args) {
        conn._args = true;
        sock.write(enc('args', 'VERSION_1_1_0', 'hostname', 'port'));
      }
      if (buf.includes('connect') && !conn.id) {
        conn.id = `$ra-${state.conns.length}`;
        sock.write(enc('ready', conn.id));
        sock.write(enc('size', '0', '800', '600'));
        sock.write(enc('sync', '1'));
      }
    });
    sock.on('error', () => {});
  });
  await new Promise((r) => guacd.listen(0, '127.0.0.1', r));
  t.after(() => guacd.close());
  const killAll = () => {
    for (const c of state.conns) {
      if (c.id && !c.dead) {
        c.dead = true;
        c.sock.write(enc('error', 'Aborted. See logs.', '515'));
      }
    }
  };

  const bridgeStub = http.createServer((req, res) => {
    if (req.url === '/probe') {
      req.resume();
      res.statusCode = 204;
      res.end();
      return;
    }
    req.resume();
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ port: 59005 }));
  });
  await new Promise((r) => bridgeStub.listen(0, '127.0.0.1', r));
  t.after(() => bridgeStub.close());

  const config = buildConfig({
    GUACD_PORT: String(guacd.address().port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    GUAC_LOG_LEVEL: 'QUIET',
    PROBE_INTERVAL_MS: '50',
  });
  const { server, gl, registry } = createGateway(config);
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  t.after(() => {
    gl.close();
    server.close();
  });

  // Browser-style client: opts into transparent re-attach (the stabilizer
  // auth relay appends this for external viewers).
  const ws = new WebSocket(
    `ws://127.0.0.1:${server.address().port}/internal/bid-ns/bid-server?reattach=1`
  );
  const c = { closed: false, sizes: 0, errors: 0, blanks: 0 };
  ws.on('close', () => {
    c.closed = true;
  });
  let buf = '';
  ws.on('message', (d) => {
    buf += d.toString();
    let i;
    while ((i = buf.indexOf('4.size,')) !== -1) {
      c.sizes++;
      buf = buf.slice(i + 7);
    }
    // The fake guacd never sends cfill: any cfill is the gateway's blank.
    while ((i = buf.indexOf('5.cfill,')) !== -1) {
      c.blanks++;
      buf = buf.slice(i + 8);
    }
    if (buf.includes('5.error,')) {
      c.errors++;
      buf = buf.replace(/5\.error,[^;]*;/g, '');
    }
    if (buf.length > 65536) {
      buf = '';
    }
  });
  await new Promise((resolve, reject) => {
    ws.on('open', resolve);
    ws.on('error', reject);
  });

  await waitFor(() => c.sizes >= 1, 'first attach display');
  assert.strictEqual(registry.size(), 1);

  // guacd connection dies; the SAME websocket must get a fresh session.
  killAll();
  await waitFor(() => c.sizes >= 2, 're-attach display on same websocket');
  assert.strictEqual(c.closed, false, 'browser websocket must never close');
  assert.ok(state.conns.filter((x) => x.id).length >= 2, 'fresh guacd connection created');

  // And again, proving it keeps working.
  killAll();
  await waitFor(() => c.sizes >= 3, 'second re-attach');
  assert.strictEqual(c.closed, false);
  // guacamole-common-js treats protocol error instructions as fatal — the
  // browser must never see them across re-attaches.
  assert.strictEqual(c.errors, 0, 'no error instruction may leak to a re-attach client');
  // Each switch-over must blank the canvas so dead-session content (e.g.
  // pre-reboot screen) doesn't bleed into the next session.
  assert.ok(c.blanks >= 2, `expected a blank per re-attach, got ${c.blanks}`);

  // Client closing stops the loop and drains the registry.
  ws.close();
  await waitFor(() => registry.size() === 0, 'registry drained after client close');
});

test('internal listener stays fail-fast without reattach param', async (t) => {
  const state = { conns: [] };
  const guacd = net.createServer((sock) => {
    const conn = { sock, id: null };
    state.conns.push(conn);
    let buf = '';
    sock.on('data', (d) => {
      buf += d.toString('utf8');
      if (buf.includes('select') && !conn._args) {
        conn._args = true;
        sock.write(enc('args', 'VERSION_1_1_0', 'hostname', 'port'));
      }
      if (buf.includes('connect') && !conn.id) {
        conn.id = `$ff-${state.conns.length}`;
        sock.write(enc('ready', conn.id));
        sock.write(enc('size', '0', '800', '600'));
      }
    });
    sock.on('error', () => {});
  });
  await new Promise((r) => guacd.listen(0, '127.0.0.1', r));
  t.after(() => guacd.close());

  const bridgeStub = http.createServer((req, res) => {
    if (req.url === '/probe') {
      req.resume();
      res.statusCode = 204;
      res.end();
      return;
    }
    req.resume();
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ port: 59006 }));
  });
  await new Promise((r) => bridgeStub.listen(0, '127.0.0.1', r));
  t.after(() => bridgeStub.close());

  const config = buildConfig({
    GUACD_PORT: String(guacd.address().port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    GUAC_LOG_LEVEL: 'QUIET',
  });
  const { server, gl } = createGateway(config);
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  t.after(() => {
    gl.close();
    server.close();
  });

  const ws = new WebSocket(
    `ws://127.0.0.1:${server.address().port}/internal/ns/ff-vmi`
  );
  let closed = false;
  ws.on('close', () => {
    closed = true;
  });
  await new Promise((resolve, reject) => {
    ws.on('open', resolve);
    ws.on('error', reject);
  });
  await waitFor(() => state.conns.length === 1 && state.conns[0].id, 'attached');

  // Kill: coordinator-style clients must see the close (fail-fast).
  state.conns[0].sock.write(enc('error', 'Aborted. See logs.', '515'));
  await waitFor(() => closed, 'fail-fast close on internal listener');
});
