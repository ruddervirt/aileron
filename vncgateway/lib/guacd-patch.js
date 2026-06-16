'use strict';

// guacamole-lite (pinned 1.2.0) hard-codes a 10s "guacd was inactive" kill in
// GuacdClient's constructor interval. Static VNC screens legitimately produce
// no guacd output for long stretches (installer boot menus, BIOS pauses,
// guest reboots), and that kill was dropping live build consoles mid-boot.
//
// The interval is inline in the constructor so it can't be overridden
// directly, but it acts by calling close(error) on the prototype — intercept
// that. Real teardown paths (socket close/error, client disconnect) pass
// different errors or none and are untouched.
//
// GUACD_INACTIVITY_MS > 0 restores a kill with that window; 0 (default)
// disables it entirely — genuine guacd death still surfaces immediately via
// the socket close/error events.
const GuacdClient = require('guacamole-lite/lib/GuacdClient');

function applyGuacdInactivityPatch(inactivityMs) {
  const origClose = GuacdClient.prototype.close;
  GuacdClient.prototype.close = function (error) {
    if (error && /inactive for too long/.test(error.message)) {
      if (inactivityMs > 0 && Date.now() > this.lastActivity + inactivityMs) {
        return origClose.call(this, error);
      }
      // Quiet screen, healthy socket: pretend there was activity so the
      // 1s-interval check stays silent for another window.
      this.lastActivity = Date.now();
      return undefined;
    }
    return origClose.call(this, error);
  };
}

// guacd announces connection death (upstream VNC lost, join target gone,
// autoretry exhausted) with an `error` instruction — but, as observed live,
// it does not reliably close the per-user sockets afterwards. Without this
// patch the member websockets stay open as zombies: browsers hang in
// "connecting", and the session registry keeps routing joins to a connection
// guacd already removed ("Connection does not exist").
//
// Treat any guacd `error` instruction as fatal for that client connection:
// forward it (so guacamole-common-js surfaces a proper tunnel error), then
// close. The error is tagged so the session registry can drop the whole
// session entry immediately.
function applyGuacdErrorPatch() {
  const origProcessInstruction = GuacdClient.prototype.processInstruction;
  GuacdClient.prototype.processInstruction = function (opcode, params) {
    origProcessInstruction.call(this, opcode, params);
    if (opcode === 'error') {
      const err = new Error(`guacd error ${params[1] || ''}: ${params[0] || ''}`);
      err.guacdError = true;
      this.close(err);
    }
  };
}

module.exports = { applyGuacdInactivityPatch, applyGuacdErrorPatch };
