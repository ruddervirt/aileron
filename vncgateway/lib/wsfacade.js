'use strict';

const { EventEmitter } = require('events');

// WsFacade stands between guacamole-lite and the real client websocket so a
// guacd session death does not close the browser connection. guacamole-lite
// only ever touches: on('message'/'close'), removeAllListeners('close'),
// readyState, OPEN/CLOSED/CLOSING, send(data, opts, cb), close(code, reason).
//
// When guacamole-lite closes the connection (guacd died), the facade detaches
// instead of closing the real socket and emits 'detached' — the gateway then
// re-attaches the same socket to a fresh session (new full frame at current
// resolution). If the real client disconnects, 'close' is forwarded to
// guacamole-lite as usual and 'detached' fires afterwards so the re-attach
// loop can stop.
class WsFacade extends EventEmitter {
  constructor(ws) {
    super();
    this.ws = ws;
    this.OPEN = ws.OPEN !== undefined ? ws.OPEN : 1;
    this.CLOSING = ws.CLOSING !== undefined ? ws.CLOSING : 2;
    this.CLOSED = ws.CLOSED !== undefined ? ws.CLOSED : 3;
    this.detached = false;
    // Last display size forwarded to the client (layer 0); used to blank the
    // screen at re-attach so stale content from this session doesn't bleed
    // into the next one.
    this.lastWidth = 0;
    this.lastHeight = 0;

    this._onMessage = (data, isBinary) => this.emit('message', data, isBinary);
    this._onClose = (...args) => {
      this.emit('close', ...args);
      this._end();
    };
    ws.on('message', this._onMessage);
    ws.on('close', this._onClose);
  }

  get readyState() {
    return this.detached ? this.CLOSED : this.ws.readyState;
  }

  send(data, opts, cb) {
    if (this.detached) {
      if (typeof cb === 'function') {
        cb();
      }
      return;
    }
    // Swallow terminal instructions: guacamole-common-js treats a protocol
    // `error`/`disconnect` as fatal and tears the client down — defeating the
    // transparent re-attach that follows. The session death still reaches the
    // gateway (guacd-patch closes the connection), and genuinely-terminal
    // failures are reported on the raw socket by attachVnc's error path.
    if (typeof data === 'string' && (data.startsWith('5.error,') || data.startsWith('10.disconnect'))) {
      if (typeof cb === 'function') {
        cb();
      }
      return;
    }
    if (typeof data === 'string' && data.startsWith('4.size,1.0,')) {
      const m = data.match(/^4\.size,1\.0,\d+\.(\d+),\d+\.(\d+);/);
      if (m) {
        this.lastWidth = Number(m[1]);
        this.lastHeight = Number(m[2]);
      }
    }
    this.ws.send(data, opts, cb);
  }

  // guacamole-lite ending the session: detach, keep the real socket open.
  close() {
    this._end();
  }

  _end() {
    if (this.detached) {
      return;
    }
    this.detached = true;
    this.ws.removeListener('message', this._onMessage);
    this.ws.removeListener('close', this._onClose);
    this.emit('detached');
  }
}

module.exports = { WsFacade };
