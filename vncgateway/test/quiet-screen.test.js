'use strict';

// A static VNC screen means guacd sends nothing for long stretches.
// guacamole-lite's stock behavior kills such connections after 10s; the
// guacd-patch must keep them alive (default GUACD_INACTIVITY_MS=0).

const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const net = require('net');
const WebSocket = require('ws');

const { buildConfig, createGateway } = require('../server');

function enc(...elems) {
  return elems.map((e) => `${[...e].length}.${e}`).join(',') + ';';
}

test('session survives >10s of guacd silence (quiet screen)', { timeout: 30000 }, async (t) => {
  // Fake guacd: handshake, one display frame, then total silence.
  const server = net.createServer((sock) => {
    let buf = '';
    sock.on('data', (d) => {
      buf += d.toString('utf8');
      if (buf.includes('select') && !sock._argsSent) {
        sock._argsSent = true;
        sock.write(enc('args', 'VERSION_1_1_0', 'hostname', 'port'));
      }
      if (buf.includes('connect') && !sock._ready) {
        sock._ready = true;
        sock.write(enc('ready', '$quiet-1'));
        sock.write(enc('size', '0', '800', '600'));
        sock.write(enc('sync', '1'));
        // ...and then nothing, like a static installer menu.
      }
    });
    sock.on('error', () => {});
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  t.after(() => server.close());

  const bridgeStub = http.createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ port: 59002 }));
  });
  await new Promise((r) => bridgeStub.listen(0, '127.0.0.1', r));
  t.after(() => bridgeStub.close());

  const config = buildConfig({
    GUACD_PORT: String(server.address().port),
    BRIDGE_URL: `http://127.0.0.1:${bridgeStub.address().port}`,
    GUAC_LOG_LEVEL: 'QUIET',
  });
  const { server: gwServer, gl, registry } = createGateway(config);
  await new Promise((r) => gwServer.listen(0, '127.0.0.1', r));
  t.after(() => {
    gl.close();
    gwServer.close();
  });

  let closed = false;
  const ws = new WebSocket(
    `ws://127.0.0.1:${gwServer.address().port}/internal/ns/quiet-vmi`
  );
  ws.on('close', () => {
    closed = true;
  });
  await new Promise((resolve, reject) => {
    ws.on('open', resolve);
    ws.on('error', reject);
  });

  // Outlive the stock 10s guacd-inactivity kill (checked every 1s).
  await new Promise((r) => setTimeout(r, 13000));

  assert.strictEqual(closed, false, 'connection must survive a quiet screen');
  assert.strictEqual(registry.size(), 1, 'session must still be registered');
  ws.close();
});
