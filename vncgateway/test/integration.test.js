'use strict';

// Integration test: real guacamole-lite wired by createGateway, against a
// fake guacd TCP server and a stub vncbridge HTTP API. Verifies the
// transparent mux end to end: first client connects, second joins, input
// flows, auth gates the external listener.

const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const net = require('net');
const WebSocket = require('ws');

const { buildConfig, createGateway } = require('../server');

// --- Guacamole wire helpers (ASCII-only is fine for tests) ---

function enc(...elems) {
  return elems.map((e) => `${[...e].length}.${e}`).join(',') + ';';
}

function tryParseOne(s) {
  let pos = 0;
  const elems = [];
  for (;;) {
    const dot = s.indexOf('.', pos);
    if (dot === -1) {
      return null;
    }
    const len = parseInt(s.slice(pos, dot), 10);
    const end = dot + 1 + len;
    if (end >= s.length) {
      return null;
    }
    elems.push(s.slice(dot + 1, end));
    const sep = s[end];
    if (sep === ';') {
      return { elems, rest: s.slice(end + 1) };
    }
    if (sep !== ',') {
      throw new Error(`bad separator ${JSON.stringify(sep)}`);
    }
    pos = end + 1;
  }
}

// --- Fake guacd ---

function startFakeGuacd() {
  const state = { connections: [] };
  state.server = net.createServer((sock) => {
    const conn = { received: [], joined: false, sock };
    state.connections.push(conn);
    let buf = '';
    sock.on('data', (d) => {
      buf += d.toString('utf8');
      for (;;) {
        let res;
        try {
          res = tryParseOne(buf);
        } catch (err) {
          sock.destroy();
          return;
        }
        if (!res) {
          return;
        }
        buf = res.rest;
        const [op, ...args] = res.elems;
        conn.received.push({ op, args });
        if (op === 'select') {
          conn.joined = args[0].startsWith('$');
          conn.selector = args[0];
          sock.write(
            enc('args', 'VERSION_1_1_0', 'hostname', 'port', 'encodings', 'color-depth', 'read-only')
          );
        } else if (op === 'connect') {
          conn.id = `$conn-${state.connections.length}`;
          sock.write(enc('ready', conn.id));
          sock.write(enc('size', '0', '1024', '768'));
          sock.write(enc('sync', '1'));
        }
      }
    });
    sock.on('error', () => {});
  });
  return new Promise((resolve) => {
    state.server.listen(0, '127.0.0.1', () => {
      state.port = state.server.address().port;
      resolve(state);
    });
  });
}

// --- Helpers ---

function waitFor(predicate, what, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  return new Promise((resolve, reject) => {
    (function poll() {
      let val;
      try {
        val = predicate();
      } catch (err) {
        return reject(err);
      }
      if (val) {
        return resolve(val);
      }
      if (Date.now() > deadline) {
        return reject(new Error(`timed out waiting for ${what}`));
      }
      setTimeout(poll, 25);
    })();
  });
}

function openWS(url) {
  const ws = new WebSocket(url);
  const messages = [];
  ws.on('message', (d) => messages.push(d.toString()));
  return new Promise((resolve, reject) => {
    ws.on('open', () => resolve({ ws, messages }));
    ws.on('error', reject);
  });
}

function wsStatus(url) {
  // Resolves with the HTTP status of a failed upgrade.
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    ws.on('unexpected-response', (req, res) => {
      res.resume();
      ws.terminate();
      resolve(res.statusCode);
    });
    ws.on('open', () => reject(new Error('upgrade unexpectedly succeeded')));
    ws.on('error', () => {});
  });
}

test('gateway end-to-end with fake guacd', async (t) => {
  const guacd = await startFakeGuacd();
  t.after(() => guacd.server.close());

  // Stub vncbridge: always hands out port 59001.
  const tunnelRequests = [];
  const bridgeStub = http.createServer((req, res) => {
    if (req.url === '/probe') {
      req.resume();
      res.statusCode = 204;
      res.end();
      return;
    }
    let body = '';
    req.on('data', (d) => (body += d));
    req.on('end', () => {
      tunnelRequests.push(JSON.parse(body));
      res.setHeader('Content-Type', 'application/json');
      res.end(JSON.stringify({ port: 59001 }));
    });
  });
  await new Promise((r) => bridgeStub.listen(0, '127.0.0.1', r));
  t.after(() => bridgeStub.close());

  const config = buildConfig({
    GUACD_PORT: String(guacd.port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    GUAC_LOG_LEVEL: 'QUIET',
  });
  const { server, gl, registry } = createGateway(config);
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  const intPort = server.address().port;
  t.after(() => {
    gl.close();
    server.close();
  });

  // --- First (internal) client creates the session ---
  const a = await openWS(`ws://127.0.0.1:${intPort}/internal/build-ns/bid-vm1`);
  await waitFor(() => guacd.connections.length === 1, 'first guacd connection');
  const guacdConn1 = guacd.connections[0];
  await waitFor(
    () => guacdConn1.received.some((i) => i.op === 'connect'),
    'guacd handshake from primary'
  );
  assert.strictEqual(guacdConn1.selector, 'vnc');
  const connectIns = guacdConn1.received.find((i) => i.op === 'connect');
  // connect args follow the advertised arg names: VERSION, hostname, port, ...
  assert.strictEqual(connectIns.args[1], '127.0.0.1');
  assert.strictEqual(connectIns.args[2], '59001', 'guacd must dial the bridge tunnel port');
  assert.deepStrictEqual(tunnelRequests, [{ namespace: 'build-ns', vmi: 'bid-vm1' }]);

  // Client receives forwarded display data.
  await waitFor(() => a.messages.length > 0, 'forwarded instructions to client A');

  // --- Second (internal) client joins the same session transparently
  // (KubeVirt allows one VNC connection per VMI, so sharing is mandatory) ---
  const b = await openWS(`ws://127.0.0.1:${intPort}/internal/build-ns/bid-vm1`);
  await waitFor(() => guacd.connections.length === 2, 'second guacd connection');
  const guacdConn2 = guacd.connections[1];
  await waitFor(() => guacdConn2.selector, 'join selector');
  assert.strictEqual(guacdConn2.selector, guacdConn1.id, 'second client must JOIN, not reconnect');
  assert.strictEqual(tunnelRequests.length, 1, 'join must not request another tunnel');
  assert.strictEqual(registry.size(), 1);

  // --- Input from client A reaches guacd ---
  a.ws.send(enc('key', '65293', '1'));
  await waitFor(
    () => guacdConn1.received.some((i) => i.op === 'key' && i.args[0] === '65293'),
    'key instruction at guacd'
  );

  // --- Debug page (no auth on the core gateway) ---
  const debugRes = await fetch(
    `http://127.0.0.1:${intPort}/internal/debug/build-ns/bid-vm1`
  );
  assert.strictEqual(debugRes.status, 200);
  assert.match(await debugRes.text(), /guacamole-common/);

  // --- Teardown bookkeeping: closing all clients empties the registry ---
  a.ws.close();
  b.ws.close();
  await waitFor(() => registry.size() === 0, 'registry drained');
});
