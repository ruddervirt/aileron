# Aileron Platform Documentation

## Helm Values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/ruddervirt/aileron` | Operator image |
| `image.tag` | `""` | Image tag (empty inherits `.Chart.AppVersion`; override locally with `--set image.tag=<sha>`) |
| `debug` | `false` | Retain finished Jobs and failed build resources for inspection |
| `failureRetention` | `30m` | How long to keep failed build resources before cleanup (Go duration; ignored when `debug=true`) |
| `buildLimits.maxCPU` | `8` | Max CPU per VM, clamped at admission; quantity, may be fractional (empty/0 = unlimited) |
| `buildLimits.maxMemory` | `16Gi` | Max memory per VM, clamped at admission (empty = unlimited) |
| `buildLimits.maxDiskSize` | `50Gi` | Max size per disk, clamped at admission (empty = unlimited) |
| `buildLimits.maxDiskCount` | `3` | Max disks per VM; exceeding fails the build (0 = unlimited) |
| `buildLimits.maxVMCount` | `4` | Max VMs per build; exceeding fails the build (0 = unlimited) |
| `egressExternal.enabled` | `true` | Enable KubeOVN egress for internet access |
| `egressExternal.cidr` | `172.17.0.0/16` | Egress subnet CIDR |
| `egressExternal.gateway` | `172.17.0.1` | Egress gateway IP |
| `egressExternal.vpn.enabled` | `false` | Route all VM egress traffic through a WireGuard tunnel instead of the node's own uplink (fails closed if the tunnel is down) |
| `egressExternal.vpn.secretName` | `""` | Secret (in the release namespace) with key `wg0.conf` holding a wg-quick style WireGuard config |
| `grading.enabled` | `true` | Enable the `GradeRequest` reconciler |
| `grading.bootWaitSeconds` | `240` | Seconds to wait after powering on a stopped VM before grading |
| `grading.maxConcurrent` | `10` | Max grades running at once cluster-wide (a slot covers booting + grading); extra requests queue in FIFO order. `0` = unlimited |
| `aileronUI.enabled` | `true` | Deploy the self-hosted web UI (unauthenticated — trusted networks only) |
| `aileronUI.service.nodePort` | `30806` | NodePort for the UI (empty = auto-assign) |
| `vncGateway.enabled` | `true` | Deploy the open-source VNC console gateway |
| `vncGateway.port` | `7778` | Cluster-internal gateway listener port |

## VPN Egress

By default, VM internet traffic exits through the node's own uplink IP. To
route it through a WireGuard tunnel instead (e.g. Mullvad or any other
WireGuard provider), so the node's own IP is never exposed to VM traffic,
create a Secret with the provider's config and point the chart at it:

```sh
kubectl create secret generic aileron-vpn -n <release-namespace> \
  --from-file=wg0.conf=./mullvad-se.conf
```

```yaml
egressExternal:
  vpn:
    enabled: true
    secretName: aileron-vpn
```

This applies cluster-wide to all VM egress traffic. If the tunnel can't be
established, VM egress traffic has no route (fails closed) rather than
falling back to the node's own uplink.
