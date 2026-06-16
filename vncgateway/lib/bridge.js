'use strict';

// ensureTunnel asks the vncbridge sidecar for a localhost TCP port piping to
// the KubeVirt VNC WebSocket of the given VMI. Idempotent on the bridge side.
async function ensureTunnel(bridgeUrl, namespace, vmi) {
  const res = await fetch(`${bridgeUrl}/tunnels`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ namespace, vmi }),
  });
  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new Error(`bridge tunnel request failed: HTTP ${res.status} ${body}`);
  }
  const { port } = await res.json();
  if (!port) {
    throw new Error('bridge returned no port');
  }
  return port;
}

// probeConsole asks the bridge whether the VMI's VNC console is reachable
// right now (a cheap dial + close).
//
// WARNING: KubeVirt allows only ONE VNC connection per VMI — a probe KICKS
// any active session. Callers must only probe when no session exists for
// the VM (waitForConsole checks registry.has() first).
async function probeConsole(bridgeUrl, namespace, vmi) {
  const res = await fetch(`${bridgeUrl}/probe`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ namespace, vmi }),
  });
  return res.ok;
}

module.exports = { ensureTunnel, probeConsole };
