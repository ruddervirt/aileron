# vncgateway — VNC console architecture

The vncgateway gives browsers and the build coordinator a shared, compressed,
always-available view of a VM's console. It replaced the raw-RFB
vncproxy/vncmux/vncroute stack in June 2026, and the original Node.js
(guacamole-lite) gateway + separate Go bridge were merged into a single Go
service shortly after.

## What runs

A single Go binary (`cmd/vncgateway`, package `internal/vncgateway`) that owns
both halves of the old design:

- the **session-sharing gateway** — one WebSocket listener, the connect-vs-join
  registry, hold-until-console, and transparent re-attach for human viewers; and
- the **in-process bridge** (`internal/vncbridge`) — per-VMI localhost TCP
  listeners that pipe to the KubeVirt VNC WebSocket subresource, plus the RFB
  geometry tracker. `EnsureTunnel`/`Probe` are direct method calls, not an HTTP
  hop.

`guacd` (the C daemon that re-encodes VNC as compressed PNG/JPEG) stays as a
separate sidecar container. The pod is therefore **two containers**.

## Open-source / proprietary split

The gateway is part of the OPEN-SOURCE aileron core: boot commands and
console viewing must work in a standalone aileron install (no external UI,
no external auth proxy). The core gateway is therefore deliberately
**authentication-free and cluster-internal** (ClusterIP only, never exposed
by an ingress). Authenticated external access is a separate, proprietary
concern: the **vncauthproxy** (a Go service in a separate, closed-source
repo — not in this tree) terminates the ingress, validates the UI's RS256
JWTs (keys embedded in that proprietary module only — they never enter
open-source images), and byte-relays the websocket to the core gateway.
Anyone running aileron standalone brings their own equivalent edge, or uses
the console only in-cluster (debug viewer, `kubectl port-forward`).

```
browser ── wss /vncgateway/{buildID}/{vm}?authorization=JWT ─► [vncauthproxy :7777]  (proprietary, out-of-tree)
                                                                  JWT + origin + claims · byte relay
                                                                  │ ws /internal/{ns}/{vmi}?reattach=1
coordinator ─ ws /internal/{namespace}/{vmiName} ────────────────┤
                                                                 ▼
                 ┌─────────────────────────────────────────────────────┐
                 │ aileron vncgateway pod (single replica, OPEN SOURCE)│
                 │                                                     │
                 │  vncgateway (Go) :7778                              │
                 │    session registry · hold/re-attach                │
                 │    in-process bridge: TCP↔WebSocket · geometry      │
                 │            │ tcp 127.0.0.1:4822                     │
                 │  guacd 1.5.5 (VNC → Guacamole protocol, PNG/JPEG)   │
                 │            │ tcp 127.0.0.1:<dynamic>                │
                 │  (guacd dials the gateway's localhost tunnel)       │
                 └────────────┼────────────────────────────────────────┘
                              ▼ wss (ServiceAccount token)
                 KubeVirt VNC subresource → virt-launcher → QEMU
```

## Requirements

1. **Console visible from the first frame.** A viewer (or the coordinator)
   may connect before the VM exists; it must see the console the moment QEMU
   has one (BIOS screen included), with no client-side retry loop.
2. **Boot commands are timing-critical.** The coordinator must start typing
   exactly when the console appears (ISO boot menus), and a keystroke must
   never be silently dropped into a dead session.
3. **Nothing disconnects — or reconnects invisibly.** Guest reboots and
   boot-time video mode changes drop the upstream VNC; the browser websocket
   must survive all of it.
4. **Pixel-correct rendering across resolution changes**, including shrinks
   (boot splash → firmware setup menu).
5. **Compressed transport.** The old stack relayed raw RFB framebuffers
   (multi-MB per frame); guacd re-encodes to PNG/JPEG and the upstream VNC
   leg negotiates a compressed encoding.

## Load-bearing facts (violate these and the system breaks)

### KubeVirt allows exactly ONE VNC connection per VMI

A new connection to the VNC subresource **silently kicks the existing one**
(the victim sees a mid-stream EOF; guacd reports "Error handling message from
VNC server"). This is easy to misdiagnose: tests with short-lived or idle
clients appear to show concurrency working, because the kicked victim only
notices when it next touches the socket.

Consequences:

- All viewers of a VM **must share one upstream connection** — the first
  client creates the guacd connection, every later client **joins** it
  (Guacamole connection sharing; see `internal/vncgateway/registry.go`).
- No out-of-band probe/watcher connection may run while a session is active.
  `waitForConsole` only probes when the registry has no session for the VM.
  Because the registry and the bridge now share a process, this invariant is a
  memory-safety property rather than a cross-process discipline.
- Anything that needs to observe the VNC stream (e.g. geometry detection)
  must do so **passively, inside the one stream** that already flows through
  the bridge (`internal/vncbridge/rfbstream.go`).

### guacd version is pinned to 1.5.5 — do not bump casually

guacd 1.6.0 rewrote its display engine and has three regressions we hit in
production (none fixed upstream as of the 1.6.1 staging branch):

| Bug | Symptom |
|---|---|
| Initial frame wiped by client-driven resize | 1.8–99.6% paint coverage; fragmented console (workaround: `disable-display-resize=true`, which we still set — harmless on 1.5.5) |
| Join display-sync degrades | After ~3 joins, joiners receive ~2% of the screen |
| Join deadlock (GUACAMOLE-2270, open) | Joined viewers freeze permanently |

1.5.5 paints 100% in every scenario we measured and its join path is the
mature pre-rewrite code. Its weakness — it does not follow guest resolution
changes — is covered by the bridge's geometry tracker instead.

If re-evaluating an upgrade: re-run the paint-coverage probe (composite
img/rect/cfill instructions into a cell grid) for fresh connections, joins
under repetition, the BIOS screen, and an in-place resize.

### guacd announces session death but doesn't close sockets

When the upstream VNC drops, guacd sends an `error` instruction (e.g. 515)
on each member's connection and then often leaves the sockets open. The
gateway's relay loop treats any guacd `error`/`disconnect` instruction as
fatal for that connection: in fail-fast mode it forwards the error and closes;
in reattach mode it swallows it and re-attaches. Either way the registry drops
the whole session entry so retries get a fresh connection instead of joining a
connection guacd already removed ("Connection does not exist").

### Guest resolution changes don't reach viewers

QEMU announces framebuffer geometry changes with the RFB `DesktopSize`
pseudo-encoding. guacd 1.5.5 ignores them; without help the viewer keeps a
stale, wrongly-sized canvas (firmware menu rendered into a corner of the old
boot splash). The bridge therefore parses the RFB stream it pipes
(`internal/vncbridge/rfbstream.go`) and **recycles the connection on any
geometry change** — every viewer is re-attached to a fresh session at the
exact new size.

For this to work, the upstream encodings must be length-parseable: the
gateway restricts guacd to `zrle copyrect` (`GUAC_ENCODINGS`). Tight is NOT
parseable; enabling it silently disables resize detection (the tracker is
fail-safe: parse desync turns detection off, never disturbs the pipe).

## How a client experiences the lifecycle

1. **Connect** (any time, even before the VM exists). Browsers go through
   the proprietary vncauthproxy (JWT validated there; it maps
   `{buildID}/{vm}` to `{namespace}/{vmiName}`, appends `?reattach=1`, and
   relays); the coordinator and in-cluster tools hit the gateway directly.
   The upgrade completes immediately; while the console doesn't exist the
   gateway holds the socket with `3.nop;` keepalives (also resets
   guacamole-common-js's 15s receive timeout) and probes the bridge every
   second, up to `CONSOLE_WAIT_MS` (default 10 min, then a clean error 519 +
   close).
2. **Attach** the instant the console probes reachable: the gateway dials
   guacd and performs the Guacamole handshake (connect for the first client,
   join otherwise), then relays the stream. Clients connect with nothing but
   a URL — there is no token (the old AES `?token=` existed only to feed
   guacamole-lite; the Go gateway owns both ends).
3. **Session death** (reboot, video mode change, recycle): clients that
   connected with `?reattach=1` (browsers, via the auth relay) keep their
   websocket; the relay loop swallows the terminal `error`/`disconnect`
   instructions, **blanks the canvas** (black rect+cfill sized from the last
   `size` seen — the next session only paints non-black regions, so stale
   content would otherwise survive), and re-attaches to a fresh session. The
   coordinator (internal listener, no reattach) instead fails fast: its
   `SendKey` errors, it reconnects, and resends the failed key.
4. **Frontend duties**: set `background: #000` on the display container
   (never-painted black regions are transparent in guacamole-common-js), and
   keep a retry-on-close loop (~1s backoff) for gateway restarts.

## Component map

| Piece | Purpose |
|---|---|
| `internal/vncgateway/gateway.go` | `Config`/`BuildConfig`, the `Gateway`, and the in-process `Bridge` interface |
| `internal/vncgateway/routes.go` | The single listener (:7778): `/healthz`, `/internal/debug/{ns}/{vmi}`, the WS upgrade |
| `internal/vncgateway/serve.go` | Per-client loop: hold/attach/re-attach orchestration, blank, fail-fast vs reattach |
| `internal/vncgateway/registry.go` | One shared session per VM: connect-vs-join decision, open/close bookkeeping, drop-on-error |
| `internal/vncgateway/guacd.go` | The guacd TCP client: the Guacamole handshake (select/args/size/audio/video/image/connect/ready) and connection join |
| `internal/guac` | The Guacamole wire codec (Encode/Decoder), shared with the coordinator client |
| `internal/vncbridge` | In-process bridge: per-VMI localhost TCP listeners piping to the KubeVirt VNC websocket; `EnsureTunnel`/`Probe`; RFB geometry tracker; idle reaping. (Its HTTP `Handler` is retained for its own tests; production calls it in-process.) |
| `internal/build/guacclient` | Minimal Go Guacamole client for the coordinator: key events, sync echo, display-gated readiness |
| `internal/vncgateway/debug.html` | guacamole-common-js viewer + screenshot button (`hack/vncview.sh` opens it) |
| vncauthproxy (proprietary, out-of-tree) | Ingress edge in a separate, closed-source repo (not in this tree, not in this chart): RS256 JWT, origin allowlist, claims-vs-path check, websocket byte relay to the core gateway |
| `chart/aileron/templates/vncgateway.yaml` | Core 2-container pod (gated on `vncGateway.enabled` only — works standalone); guacd binds loopback (no auth → must never leave the pod) |

## Guacamole error codes seen in the wild

- **519** — console/VMI not up yet (guacd autoretry exhausted). Normal while
  a VM is importing/booting.
- **515** — established VNC dropped (reboot, video mode re-init).
- **776** — client never acked `sync`; guacd kicks it after ~15s. The gateway
  relays `sync` transparently in the client→guacd direction; well-behaved
  clients (guacamole-common-js, our Go client) always ack.

## Debugging

- `hack/vncview.sh <ns>/<vmiName>` — port-forwards and opens the built-in
  debug viewer.
- Gateway logs (`vncgateway` container): session opens/joins/drops,
  re-attaches, hold timeouts, tunnel dials, KubeVirt errors,
  `framebuffer size changed; recycling` events (the bridge logs through the
  same process now).
- The protocol is plain text over the websocket: `wscat` against
  `/internal/{ns}/{vmi}` shows instructions directly (echo `4.sync,…;` back
  or guacd kicks you with 776).

## History / postmortem pointers

The shape of this system is the residue of real incidents — in rough order:
boot commands typed into never-attached sessions (guacd sends `ready` before
its VNC leg exists → the Go client gates readiness on display output, not
control instructions); zombie sessions after guacd errors; sessions killed
every 10s on static installer menus (guacamole-lite's hardcoded inactivity
timer — gone with the Node rewrite: the Go relay has no inactivity kill);
guacd 1.6's display regressions; and the per-viewer-connection refactor that
was reverted when the one-connection-per-VMI kick behavior was finally proven.
Details with measurements live in the session memory and the git history of
`internal/vncgateway/`, `internal/vncbridge/`, and the former `vncgateway/`
Node tree.
