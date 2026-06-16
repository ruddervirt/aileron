'use strict';

// Console viewer: connects guacamole-common-js to aileron-ui's /vnc reverse
// proxy, which relays to the cluster-internal vncgateway. Adapted from
// vncgateway/public/debug.html. Auto-reconnects with ~1s backoff because the
// upstream VNC session can drop (e.g. a boot-time video-mode switch).

(function () {
  const params = new URLSearchParams(location.search);
  const ns = params.get('ns');
  const vmi = params.get('vmi');
  const name = params.get('name') || vmi || '';

  const stateEl = document.getElementById('state');
  const viewport = document.getElementById('viewport');
  const displayEl = document.getElementById('display');
  const shotBtn = document.getElementById('shot');
  // Only usable once actually connected to a console (see CONNECTED below).
  shotBtn.disabled = true;
  document.getElementById('name').textContent = name + (ns ? '  (' + ns + ')' : '');

  if (!ns || !vmi) {
    stateEl.textContent = 'error: missing ns/vmi query params';
    return;
  }

  const wsProto = location.protocol === 'https:' ? 'wss://' : 'ws://';
  const wsUrl = wsProto + location.host + '/vnc/' +
    encodeURIComponent(ns) + '/' + encodeURIComponent(vmi);

  const stateNames = ['idle', 'connecting', 'waiting', 'connected',
    'disconnecting', 'disconnected'];

  let client = null;
  let keyboard = null;
  let reconnectTimer = null;
  let closed = false;
  let currentScale = 1;

  // guacamole-common-js renders at the guest's native resolution and does not
  // auto-scale. Fit the display into the viewport, preserving aspect ratio, and
  // size #display to the scaled dimensions so flexbox can center it. Re-run on
  // guest resolution changes (display.onresize) and browser resizes.
  function rescale() {
    if (!client) return;
    const display = client.getDisplay();
    const w = display.getWidth();
    const h = display.getHeight();
    if (!w || !h) return;
    const scale = Math.min(viewport.clientWidth / w, viewport.clientHeight / h);
    currentScale = scale;
    display.scale(scale);
    displayEl.style.width = (w * scale) + 'px';
    displayEl.style.height = (h * scale) + 'px';
  }
  window.addEventListener('resize', rescale);

  function cleanup() {
    shotBtn.disabled = true;
    if (client) {
      try { client.disconnect(); } catch (_) { /* ignore */ }
      client = null;
    }
    if (keyboard) { keyboard.onkeydown = keyboard.onkeyup = null; keyboard = null; }
  }

  function scheduleReconnect() {
    if (closed || reconnectTimer) return;
    reconnectTimer = setTimeout(() => { reconnectTimer = null; connect(); }, 1000);
  }

  function connect() {
    cleanup();
    displayEl.innerHTML = '';

    const tunnel = new Guacamole.WebSocketTunnel(wsUrl);
    // A live console is often visually static for long stretches (idle guest,
    // blanked VGA console after ~10min, an installer menu), during which guacd
    // sends no frames. guacamole's default 15s receive timeout would then tear
    // the tunnel down and trigger an endless reconnect loop that also re-blanks
    // the canvas. Extend it so idle consoles stay put; a genuine socket close
    // still fires onclose -> reconnect. (Must stay below setTimeout's ~24.8d
    // cap, beyond which it would fire immediately.)
    tunnel.receiveTimeout = 86400000; // 24h, effectively "don't time out idle"
    client = new Guacamole.Client(tunnel);

    client.onstatechange = function (s) {
      stateEl.textContent = stateNames[s] || s;
      // 3 === Guacamole.Client.State.CONNECTED — only then is the display live.
      shotBtn.disabled = s !== 3;
    };
    client.onerror = function (err) {
      stateEl.textContent = 'error: ' + ((err && err.message) || JSON.stringify(err));
      shotBtn.disabled = true;
      scheduleReconnect();
    };
    tunnel.onerror = function () { scheduleReconnect(); };
    tunnel.onstatechange = function (s) {
      // 2 === Guacamole.Tunnel.State.CLOSED
      if (s === 2) scheduleReconnect();
    };

    const display = client.getDisplay();
    display.onresize = rescale; // guest resolution change -> refit
    displayEl.style.width = '';
    displayEl.style.height = '';
    displayEl.appendChild(display.getElement());
    client.connect('');

    keyboard = new Guacamole.Keyboard(document);
    keyboard.onkeydown = function (keysym) { client.sendKeyEvent(1, keysym); };
    keyboard.onkeyup = function (keysym) { client.sendKeyEvent(0, keysym); };

    const mouse = new Guacamole.Mouse(displayEl);
    mouse.onmousedown = mouse.onmouseup = mouse.onmousemove = function (state) {
      // The display is CSS-scaled; map viewport pixels back to guest pixels.
      if (currentScale && currentScale !== 1) {
        state.x = Math.round(state.x / currentScale);
        state.y = Math.round(state.y / currentScale);
      }
      client.sendMouseState(state);
    };
  }

  shotBtn.addEventListener('click', function () {
    if (!client || shotBtn.disabled) return;
    const canvas = client.getDisplay().flatten();
    const a = document.createElement('a');
    a.href = canvas.toDataURL('image/png');
    a.download = (name || 'console') + '.png';
    a.click();
  });

  window.addEventListener('unload', function () { closed = true; cleanup(); });

  connect();
})();
