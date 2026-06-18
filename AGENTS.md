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

### CRDs

- **VirtualMachineBuild** — Creates VMs from source images, provisions them, captures disk snapshots as templates
- **VirtualMachineClone** — Clones a build's template VMs via CSI volume snapshots into running instances
- **VirtualMachineNamespace** — Internal bookkeeping CR (one per build, tracks the buildID)
- **GradeRequest** — Runs per-VM commands over the KubeVirt serial console and records results (grading method resolved from the VM's `ruddervirt.io/os` label); reconciler built into the core manager

### Resource Naming

All resources for a build use `{buildID}-` prefix `vm-`. All resources for a clone use `{cloneID}-` prefix `ns-`.

## Build & Package

This repo only builds/versions the images and produces the Helm chart — it never
deploys to a cluster. Deployment is owned by the consuming environments, which
pull the published images and chart.

```bash
make build      # build SHA-tagged images locally
make push       # build + push images to the registry
make helm-publish CHART_VERSION=1.2.3   # package + push the versioned OCI chart
```

`build` and `push` chain to `generate`. The image tag is auto-generated from the
git SHA. Helm chart is at `chart/aileron/`.

**Releases (how zones get deployed):** push a `vX.Y.Z` git tag. `.github/workflows/release.yml`
then builds every image tagged `:vX.Y.Z` and publishes a versioned OCI Helm chart to
`oci://ghcr.io/ruddervirt/charts/aileron` (chart version `X.Y.Z`, appVersion `vX.Y.Z`).
The chart is self-versioning: image tags resolve from `.Chart.AppVersion`
(`aileron.imageTag` in `_helpers.tpl`), so a zone installs with no `--set image.*`.

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
`make generate` picks it up.
