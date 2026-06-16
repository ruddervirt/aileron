'use strict';

// vncgateway: transparent VNC viewing for builds and clones — the OPEN
// SOURCE core. Architecture, requirements, and the reasoning behind every
// quirk: vncgateway.md (this directory). Read it before changing this file.
//
// This service is deliberately authentication-free and must only be exposed
// inside the cluster (ClusterIP). External/authenticated access is a
// separate concern layered on top — the proprietary stabilizer vncauthproxy
// validates JWTs and byte-relays here. Clients (browsers via that relay, the
// build coordinator's Go guacclient, and in-cluster debug viewers) connect
// with nothing but a URL; this server mints guacamole-lite tokens
// internally, so clients never see tokens or the AES key.
//
// KubeVirt allows exactly ONE VNC connection per VMI (a new one kicks the
// old), so the first client creates the guacd connection and every later
// client JOINS it. guacd re-encodes the VNC stream as compressed PNG/JPEG,
// and the vncbridge sidecar adapts guacd's plain-TCP VNC dial to the
// KubeVirt WebSocket subresource, recycling connections when the guest
// resolution changes.
//
//   any client ── ws /internal/{namespace}/{vmiName}[?reattach=1] ──┐
//                                                                   ▼
//                  this app ── tcp ──► guacd ── tcp ──► vncbridge ── wss ──► KubeVirt
//
// ?reattach=1 keeps the client's websocket open across session deaths
// (transparent re-attach + canvas blank) — used for human viewers. Without
// it, session death closes the socket immediately (fail-fast), which the
// coordinator requires so keystrokes are never typed into a dead session.

const fs = require('fs');
const http = require('http');
const path = require('path');
const GuacamoleLite = require('guacamole-lite');

const { applyGuacdInactivityPatch, applyGuacdErrorPatch } = require('./lib/guacd-patch');
const { TokenFactory } = require('./lib/tokens');
const { SessionRegistry } = require('./lib/registry');
const { ensureTunnel, probeConsole } = require('./lib/bridge');
const { parseTwoSegments } = require('./lib/path');
const { WsFacade } = require('./lib/wsfacade');

const INTERNAL_PREFIX = '/internal/';
const INTERNAL_DEBUG_PREFIX = '/internal/debug/';

function log(...args) {
  console.log(new Date().toISOString(), ...args);
}

function buildConfig(env) {
  return {
    listenPort: Number(env.LISTEN_PORT || 7778),
    guacdHost: env.GUACD_HOST || '127.0.0.1',
    guacdPort: Number(env.GUACD_PORT || 4822),
    bridgeUrl: env.BRIDGE_URL || 'http://127.0.0.1:9190',
    logLevel: env.GUAC_LOG_LEVEL || 'NORMAL',
    maxInactivityTime: Number(env.MAX_INACTIVITY_MS || 0),
    // 0 disables guacamole-lite's guacd-silence kill (see lib/guacd-patch.js).
    guacdInactivityMs: Number(env.GUACD_INACTIVITY_MS || 0),
    // While a VM's console doesn't exist yet (importing, scheduling, booting),
    // client connections are HELD with keepalive nops and attached the moment
    // the console probes reachable — no client-side retry loop needed.
    consoleWaitMs: Number(env.CONSOLE_WAIT_MS || 10 * 60 * 1000),
    probeIntervalMs: Number(env.PROBE_INTERVAL_MS || 1000),
    // Settings for the upstream VNC leg (guacd -> vncbridge -> KubeVirt) and
    // the client-facing encode. encodings makes libvncclient negotiate Tight
    // instead of the Raw-dominant stream the old mux was stuck with.
    connectSettings: {
      autoretry: env.GUAC_AUTORETRY || '3',
      'color-depth': env.GUAC_COLOR_DEPTH || '24',
      // Restricted to encodings the bridge's passive geometry tracker can
      // parse (ZRLE is length-prefixed; tight is not parseable).
      encodings: env.GUAC_ENCODINGS || 'zrle copyrect',
      'quality-level': env.GUAC_QUALITY_LEVEL || '8',
      'compress-level': env.GUAC_COMPRESS_LEVEL || '2',
      cursor: env.GUAC_CURSOR || 'remote',
      // guacd 1.6.0's client-driven resize wipes the initial frame (known
      // upstream regression, maintainer-confirmed workaround). QEMU ignores
      // client-driven resize anyway; server-driven guest resizes still work.
      ...(env.GUAC_DISABLE_DISPLAY_RESIZE === 'false'
        ? {}
        : { 'disable-display-resize': 'true' }),
      ...(env.GUAC_FORCE_LOSSLESS === 'true' ? { 'force-lossless': 'true' } : {}),
    },
    // Joiners get full input so humans and the coordinator can both type.
    joinSettings: { 'read-only': 'false' },
  };
}

function rejectSocket(socket, code, reason) {
  if (socket.writable) {
    socket.write(`HTTP/1.1 ${code} ${reason}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
  }
  socket.destroy();
}

function sendText(res, code, body, contentType = 'text/plain') {
  res.writeHead(code, { 'Content-Type': contentType });
  res.end(body);
}

// createGateway wires the two HTTP listeners, the session registry, and
// guacamole-lite. Exported for tests; start() below runs it for real.
function createGateway(config, overrides = {}) {
  applyGuacdInactivityPatch(config.guacdInactivityMs);
  applyGuacdErrorPatch();
  const tokens = new TokenFactory();
  const registry = new SessionRegistry({
    ensureTunnel:
      overrides.ensureTunnel || ((ns, vmi) => ensureTunnel(config.bridgeUrl, ns, vmi)),
    connectSettings: config.connectSettings,
    joinSettings: config.joinSettings,
    log,
  });

  // wsOptions must own a `server` property: guacamole-lite passes it to ws
  // verbatim only in that case (otherwise it adds port:8080 and ws would
  // start its own listener instead of noServer mode).
  const gl = new GuacamoleLite(
    { noServer: true, server: undefined },
    { host: config.guacdHost, port: config.guacdPort },
    {
      crypt: tokens.cryptOptions(),
      log: { level: config.logLevel },
      maxInactivityTime: config.maxInactivityTime,
    }
  );

  // The ws connection object is tagged in handleUpgrade below; the token copy
  // is a fallback in case guacamole-lite ever stops exposing webSocket.
  const keyOf = (cc) =>
    (cc.webSocket && cc.webSocket.vncgwKey) ||
    (cc.connectionSettings && cc.connectionSettings.vncgwKey);
  const isPrimary = (cc) =>
    (cc.webSocket && cc.webSocket.vncgwPrimary) ||
    (cc.connectionSettings && cc.connectionSettings.vncgwPrimary) === true;
  gl.on('open', (cc) => registry.onOpen(keyOf(cc), cc));
  gl.on('close', (cc, err) => registry.onClose(keyOf(cc), cc, isPrimary(cc), err));

  const debugHTML = fs.readFileSync(path.join(__dirname, 'public', 'debug.html'), 'utf8');

  const NOP = '3.nop;';
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  function encInstr(...elems) {
    return elems.map((e) => `${[...String(e)].length}.${e}`).join(',') + ';';
  }

  function sendGuacError(ws, message, code) {
    try {
      ws.send(encInstr('error', message, code));
    } catch (err) {
      // socket already gone
    }
  }

  // blankDisplay paints layer 0 opaque black (composite mode 14 = SRC_OVER)
  // so content from a dead session doesn't bleed into the next one.
  function blankDisplay(ws, width, height) {
    try {
      ws.send(encInstr('rect', 0, 0, 0, width, height));
      ws.send(encInstr('cfill', 14, 0, 0, 0, 0, 255));
    } catch (err) {
      // socket already gone
    }
  }

  // acceptVnc completes the WebSocket upgrade immediately, then attaches the
  // client to a session with a server-minted token. The client never sees
  // tokens or the AES key.
  async function acceptVnc(req, socket, head, namespace, vmi, preservedQuery, reattach) {
    gl.webSocketServer.handleUpgrade(req, socket, head, (ws) => {
      attachVnc(ws, req, namespace, vmi, preservedQuery, reattach).catch((err) => {
        log(`attach failed for ${namespace}/${vmi}: ${err.message}`);
        sendGuacError(ws, 'Console unavailable', '519');
        ws.close();
      });
    });
  }

  // waitForConsole holds the connection until the VM's console exists
  // (keepalive nops reset guacamole-common-js's 15s receive timeout and keep
  // proxies warm). Returns false if the client disconnected while waiting;
  // throws on timeout.
  //
  // INVARIANT: probing kicks any active VNC session (KubeVirt allows one
  // connection per VMI), so we only probe while registry.has() is false —
  // i.e. while there is provably nothing to kick.
  async function waitForConsole(ws, namespace, vmi) {
    if (registry.has(namespace, vmi)) {
      return true;
    }
    const deadline = Date.now() + config.consoleWaitMs;
    let closed = false;
    const onClose = () => {
      closed = true;
    };
    ws.once('close', onClose);
    ws.send(NOP);
    try {
      for (;;) {
        if (closed) {
          return false; // client gave up while waiting
        }
        if (registry.has(namespace, vmi)) {
          return true; // another client just created the session
        }
        let up = false;
        try {
          up = await probeConsole(config.bridgeUrl, namespace, vmi);
        } catch (err) {
          // bridge hiccup: treat as not-up and keep waiting
        }
        if (up) {
          return !closed;
        }
        if (Date.now() > deadline) {
          throw new Error(`console did not appear within ${config.consoleWaitMs}ms`);
        }
        ws.send(NOP);
        await sleep(config.probeIntervalMs);
      }
    } finally {
      ws.removeListener('close', onClose);
    }
  }

  // attachVnc connects a client to a session; the console appears the moment
  // the VM has a first frame. With reattach (browsers), the SAME websocket is
  // transparently re-attached to a fresh session whenever the underlying
  // guacd connection dies (QEMU drops VNC clients on boot-time display
  // re-inits) — the viewer sees a brief freeze and then a fresh full frame at
  // the current resolution, never a disconnect. Without reattach (the
  // coordinator), session death surfaces immediately so typed keys are never
  // silently dropped.
  async function attachVnc(ws, req, namespace, vmi, preservedQuery, reattach) {
    for (;;) {
      if (!(await waitForConsole(ws, namespace, vmi))) {
        return; // client left while waiting
      }

      const tokenObj = await registry.acquire(namespace, vmi);
      let url = '/?token=' + encodeURIComponent(tokens.encrypt(tokenObj));
      if (preservedQuery) {
        url += '&' + preservedQuery;
      }
      req.url = url;

      if (!reattach) {
        ws.vncgwKey = tokenObj.vncgwKey;
        ws.vncgwPrimary = tokenObj.vncgwPrimary === true;
        gl.webSocketServer.emit('connection', ws, req);
        return;
      }

      const facade = new WsFacade(ws);
      facade.vncgwKey = tokenObj.vncgwKey;
      facade.vncgwPrimary = tokenObj.vncgwPrimary === true;
      const ended = new Promise((resolve) => facade.once('detached', resolve));
      gl.webSocketServer.emit('connection', facade, req);
      await ended;

      if (ws.readyState !== ws.OPEN) {
        return; // client disconnected
      }
      // Blank the canvas: the next session only paints non-black regions, so
      // leftovers from the dead session would otherwise persist (e.g. across
      // a guest reboot).
      if (facade.lastWidth > 0 && facade.lastHeight > 0) {
        blankDisplay(ws, facade.lastWidth, facade.lastHeight);
      }
      log(`session ended for ${namespace}/${vmi}; re-attaching viewer`);
      await sleep(500);
    }
  }

  // Single listener: no auth by design (ClusterIP-only; external access goes
  // through the stabilizer vncauthproxy). Used by the coordinator, the auth
  // relay, and in-cluster debug viewers.
  const server = http.createServer((req, res) => {
    const urlObj = new URL(req.url, 'http://localhost');
    if (req.method === 'GET' && urlObj.pathname === '/healthz') {
      sendText(res, 200, 'ok');
      return;
    }
    if (req.method === 'GET' && urlObj.pathname.startsWith(INTERNAL_DEBUG_PREFIX)) {
      if (parseTwoSegments(urlObj.pathname, INTERNAL_DEBUG_PREFIX)) {
        sendText(res, 200, debugHTML, 'text/html');
      } else {
        sendText(res, 404, 'Not Found');
      }
      return;
    }
    sendText(res, 404, 'Not Found');
  });

  server.on('upgrade', (req, socket, head) => {
    const urlObj = new URL(req.url, 'http://localhost');
    if (urlObj.pathname.startsWith(INTERNAL_DEBUG_PREFIX)) {
      rejectSocket(socket, 404, 'Not Found');
      return;
    }
    const segments = parseTwoSegments(urlObj.pathname, INTERNAL_PREFIX);
    if (!segments) {
      rejectSocket(socket, 404, 'Not Found');
      return;
    }
    // Default is fail-fast (the coordinator must see session death
    // immediately so keys are never typed into a void); human viewers (and
    // the stabilizer auth relay on their behalf) opt into transparent
    // re-attach with ?reattach=1.
    const reattach = urlObj.searchParams.get('reattach') === '1';
    urlObj.searchParams.delete('reattach');
    acceptVnc(req, socket, head, segments[0], segments[1], urlObj.searchParams.toString(), reattach);
  });

  return { server, gl, registry, tokens };
}

function start() {
  const config = buildConfig(process.env);
  const { server } = createGateway(config);

  server.listen(config.listenPort, () => {
    log(`vncgateway listening on :${config.listenPort} (prefix ${INTERNAL_PREFIX})`);
  });
}

if (require.main === module) {
  start();
}

module.exports = { buildConfig, createGateway };
