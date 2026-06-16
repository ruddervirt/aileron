'use strict';

// Reproduces the production failure: guacd announces connection death with an
// `error` instruction but leaves the user sockets open. The gateway must
// close every member websocket immediately and invalidate the session entry
// so retrying clients get a fresh connection instead of joining a corpse.

const test = require('node:test');
const assert = require('node:assert');
const crypto = require('crypto');
const fs = require('fs');
const http = require('http');
const net = require('net');
const os = require('os');
const path = require('path');
const WebSocket = require('ws');

const { buildConfig, createGateway } = require('../server');

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
    pos = end + 1;
  }
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

test('guacd error kills members, invalidates session, retry reconnects', async (t) => {
  // Fake guacd. Live connection sockets are recorded; killConnection() sends
  // an error instruction WITHOUT closing the sockets (matching observed guacd
  // behavior). Joins to a killed id get an error reply, also without close.
  const state = { conns: [], liveId: null };
  const guacd = net.createServer((sock) => {
    const conn = { sock, joined: false, dead: false };
    state.conns.push(conn);
    let buf = '';
    sock.on('data', (d) => {
      buf += d.toString('utf8');
      for (;;) {
        const res = tryParseOne(buf);
        if (!res) {
          return;
        }
        buf = res.rest;
        const [op, ...args] = res.elems;
        if (op === 'select') {
          conn.joined = args[0].startsWith('$');
          conn.staleJoin = conn.joined && args[0] !== state.liveId;
          sock.write(enc('args', 'VERSION_1_1_0', 'hostname', 'port'));
        } else if (op === 'connect') {
          if (conn.staleJoin) {
            // "Connection does not exist" — error, socket left open.
            sock.write(enc('error', 'Connection does not exist', '519'));
            return;
          }
          if (!conn.joined) {
            state.liveId = `$conn-${state.conns.length}`;
          }
          conn.id = state.liveId;
          sock.write(enc('ready', conn.id));
          sock.write(enc('size', '0', '800', '600'));
          sock.write(enc('sync', '1'));
        }
      }
    });
    sock.on('error', () => {});
  });
  await new Promise((r) => guacd.listen(0, '127.0.0.1', r));
  t.after(() => guacd.close());

  const killConnection = () => {
    state.liveId = null;
    for (const c of state.conns) {
      if (!c.dead && c.id) {
        c.dead = true;
        // Error instruction, socket deliberately left open (zombie).
        c.sock.write(enc('error', 'Aborted. See logs.', '515'));
      }
    }
  };

  const bridgeStub = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ port: 59003 }));
  });
  await new Promise((r) => bridgeStub.listen(0, '127.0.0.1', r));
  t.after(() => bridgeStub.close());

  const keysDir = fs.mkdtempSync(path.join(os.tmpdir(), 'vncgw-death-'));
  const { publicKey } = crypto.generateKeyPairSync('rsa', {
    modulusLength: 2048,
    publicKeyEncoding: { type: 'spki', format: 'pem' },
    privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  });
  fs.writeFileSync(path.join(keysDir, 'test.pem'), publicKey);

  const config = buildConfig({
    GUACD_PORT: String(guacd.address().port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    KEYS_DIR: keysDir,
    GUAC_LOG_LEVEL: 'QUIET',
  });
  const { server, gl, registry } = createGateway(config);
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  t.after(() => {
    gl.close();
    server.close();
  });
  const base = `ws://127.0.0.1:${server.address().port}/internal/ns/death-vmi`;

  const openClient = async () => {
    const ws = new WebSocket(base);
    const c = { ws, closed: false, gotError: false };
    ws.on('close', () => {
      c.closed = true;
    });
    ws.on('message', (d) => {
      if (d.toString().includes('5.error')) {
        c.gotError = true;
      }
    });
    await new Promise((resolve, reject) => {
      ws.on('open', resolve);
      ws.on('error', reject);
    });
    return c;
  };

  // Coordinator-style primary + UI-style joiner.
  const primary = await openClient();
  await waitFor(() => registry.size() === 1, 'session open');
  const joiner = await openClient();
  await waitFor(() => state.conns.filter((c) => c.id).length === 2, 'join at guacd');

  // The VM's video mode switch kills the VNC leg: guacd errors, sockets stay.
  killConnection();

  // Both member websockets must be closed promptly with a forwarded error,
  // and the session entry must be gone.
  await waitFor(() => primary.closed && joiner.closed, 'members closed after guacd error');
  assert.ok(primary.gotError, 'primary should receive the error instruction');
  assert.ok(joiner.gotError, 'joiner should receive the error instruction');
  await waitFor(() => registry.size() === 0, 'session entry dropped');

  // A retrying client must get a FRESH connection (select vnc, not a stale
  // join) and work.
  const retry = await openClient();
  await waitFor(() => registry.size() === 1, 'fresh session after retry');
  await waitFor(() => state.conns.length === 3 && state.conns[2].id, 'retry handshake at guacd');
  assert.strictEqual(state.conns[2].joined, false, 'retry must be a new connection, not a join');
  assert.ok(!retry.closed, 'retry connection must stay open');
  retry.ws.close();
});
