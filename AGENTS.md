## Approach
- Think before acting. Read existing files before writing code.
- Be concise in output but thorough in reasoning.
- Prefer editing over rewriting whole files.
- Do not re-read files you have already read unless the file may have changed.
- Test your code before declaring done.
- Write unit and e2e tests for all important business logic
- No sycophantic openers or closing fluff.
- Keep solutions simple and direct. No over-engineering.
- If unsure: say so. Never guess or invent file paths.
- User instructions always override this file.

## Efficiency
- Read before writing. Understand the problem before coding.
- No redundant file reads. Read each file once.
- One focused coding pass. Avoid write-delete-rewrite cycles.
- Test once, fix if needed, verify once. No unnecessary iterations.
- Budget: 50 tool calls maximum. Work efficiently.


# Aileron

Kubernetes operator for building VM disk images and cloning them on KubeVirt.

## Architecture

Everything runs in `ruddervirt-system`. Builds and clones each get a unique CUID2 identifier (`vm-*` for builds, `ns-*` for clones) used as a prefix for all resources.

### CRDs

- **VirtualMachineBuild** — Creates VMs from source images, provisions them, captures disk snapshots as templates
- **VirtualMachineClone** — Clones a build's template VMs via CSI volume snapshots into running instances
- **VirtualMachineNamespace** — Internal bookkeeping CR (one per build, tracks the buildID)

### Build Pipeline

```
Pending → Networking → Building → CapturingDisks → [Exporting] → TemplateProvisioning → Succeeded
```

Per-VM phases (parallel within Building):
```
Pending → SourceImporting → Booting → [BootCommand] → Provisioning → ShuttingDown → DiskCaptured → Succeeded
```

### Clone Pipeline

```
Pending → Validating → SnapshotSelection → VolumeProvisioning → Networking → VMProvisioning → Ready
```

### Resource Naming

All resources for a build use `{buildID}-` prefix. All resources for a clone use `{cloneID}-` prefix.

| Resource | Build (`vm-abc123`) | Clone (`ns-xyz789`) |
|---|---|---|
| Build VM (ephemeral → reused for template) | `vm-abc123-{vmName}` | `ns-xyz789-{vmName}` |
| Output Disk (DV/PVC) | `vm-abc123-out-{vmName}` | `ns-xyz789-out-{vmName}` |
| Source Disk (DV) | `vm-abc123-src-{vmName}` | — |
| NAD | `vm-abc123-{subnet}-nad` | `ns-xyz789-{subnet}-nad` |
| Subnet | `vm-abc123-{subnet}` | `ns-xyz789-{subnet}` |
| VPC | `vm-abc123-{vpc}` | `ns-xyz789-{vpc}` |
| Egress Gateway | `vm-abc123-{vpc}-egress` | `ns-xyz789-{vpc}-egress` |

The ephemeral build VM and the halted template VM share the same name —
`TemplateProvisioner.Handle` deletes the ephemeral VM, waits for its finalizers
to drop it from etcd (via `buildVMsFullyDeleted`), then recreates it as a
halted template VM pointing at the output PVC. Every per-VM resource (VM, DV,
coordinator job, capture job, template VM) is labeled with
`ruddervirt.io/vm={vmName}`, which is how the clone layer derives clone-side
names without parsing the template VM's full name.
| SSH Secret | `vm-abc123-ssh` | — |
| Relay Pod | `vm-abc123-relay` | — |

## Build & Deploy

Local dev loop (builds SHA-tagged images and helm-upgrades the working tree's chart):

```bash
make install    # docker build + push + helm upgrade (uses KUBE_CONTEXT=direct)
```

`build`, `push`, `install` chain to the previous; the image tag is auto-generated from
the git SHA. Helm chart is at `chart/aileron/`.

**Releases (how zones get deployed):** push a `vX.Y.Z` git tag. `.github/workflows/release.yml`
then builds every image tagged `:vX.Y.Z` and publishes a versioned OCI Helm chart to
`oci://ghcr.io/ruddervirt/charts/aileron` (chart version `X.Y.Z`, appVersion `vX.Y.Z`).
The chart is self-versioning: image tags resolve from `.Chart.AppVersion`
(`aileron.imageTag` in `_helpers.tpl`), so a zone installs with no `--set image.*`.

Each deployment zone **pulls** the chart itself — CI never connects into a zone:

```bash
VERSION=X.Y.Z hack/zone-deploy.sh zone-values.yaml   # run inside the zone
```

`zone-values.yaml` holds that zone's overrides (resources, nodePorts, egress CIDR, ...)
and is kept in the zone, not the repo. Pushes to `main` run CI only (`ci.yml`:
lint/test/e2e + `main-<sha>`/`latest` dev images); they do not deploy.

The stabilizer (NATS-to-UI bindings) is a separate, self-contained project under
`stabilizer/` with its own Dockerfile, bake, Makefile, and Helm chart. `make install`
here deploys only the aileron core; build/deploy the stabilizer with
`make -C stabilizer install` (it assumes aileron is already installed on the cluster).

## Project Structure

```
cmd/main.go                          Manager entry point
api/v1alpha1/*_types.go              CRD type definitions
internal/controller/                 Reconcilers (build + clone)
internal/build/                      Build phases (VM boot, provisioning, capture, template, network, relay, SSH)
internal/clone/                      Clone phases (validate, snapshot, volume, network rewire)
internal/network/                    KubeOVN VPC/subnet/egress management
internal/namespace/                  CUID2 ID generation, namespace helpers
chart/aileron/                       Helm chart (CRDs, RBAC, deployment, secrets)
test/integration/                    Integration test manifests (builds + clones)
hack/                                Audit policy, direct kubeconfig, boilerplate
```

## Key Patterns

- **Single namespace** — all builds and clones share `ruddervirt-system`, isolated by ID prefix
- **Labels for association** — `ruddervirt.io/build-id`, `ruddervirt.io/clone` (not ownerReferences)
- **List+delete for cleanup** — `DeleteAllOf` requires `deletecollection` RBAC verb, so we list by label then delete individually
- **Non-blocking reconciler** — check-and-requeue (e.g., `IsRelayReady`), never block waiting
- **Relay-only SSH** — all VM access through a relay pod, no masquerade NICs
- **Clone volumes** — PVC created directly from CSI snapshot (same namespace, no PV transfer)
- **Network topology preserved** — template VMs carry a JSON annotation with the build's VPC/subnet/NIC topology; clones recreate equivalent resources with their own prefix
- **VM resource requests** — configurable via Helm values (`vmResources.cpu`, `vmResources.memory`), controls scheduler concurrency

## Critical Rules

- `BuildNS()` is for Kubernetes namespace lookups. `BuildID()` is for resource naming. Never use `BuildNS` as a name prefix.
- `client.Update()` on spec replaces the in-memory object — persist status BEFORE spec updates
- KubeVirt: `spec.running` and `spec.runStrategy` are mutually exclusive — always use `runStrategy`
- KubeOVN: subnet cleanup MUST happen before VPC deletion
- Never force-remove finalizers — always let cleanup run

## After Editing Types

```bash
make generate   # runs controller-gen (deepcopy + CRDs) AND syncs chart/aileron/templates/crds/
make verify-crds   # regenerates into a temp dir and diffs; CI uses this to catch drift
```

`make generate` is also a prerequisite of `make build`, so any type edit is picked
up on the next build. CRDs are shipped by the Helm chart (not `kubectl apply`) —
the chart's `templates/crds/` directory mirrors `config/crd/bases/` (controller-gen
output). When adding a new CRD, put the type in `./api/` or `./internal/` and
`make generate` picks it up. (The stabilizer project vendors its own copy of these
CRD YAMLs under `stabilizer/config/crd/` to build its API contract — re-sync with
`make -C stabilizer sync-crds` then `make -C stabilizer generate` when CRDs change.)
