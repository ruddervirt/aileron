# Aileron end-user documentation

This is the end-user reference for the resources Aileron exposes: how to write
them, what their status fields mean, and the conventions (labels, naming,
lifecycle) that connect them together. It's written for anything that creates
or watches these resources programmatically — a fleet-management controller,
a UI backend, or a human operator — not just for someone hand-writing YAML.

**The schema Aileron installs is authoritative for field-level validation**
(required/optional fields, types, enums, defaults). If you have direct
Kubernetes access, `kubectl explain <kind>.spec` shows it; if you're
integrating through a UI or API layer instead, that layer should surface
equivalent validation. This guide covers what the schema alone can't:
workflow, cross-resource relationships, label/annotation conventions, and
status/phase semantics.

All resources below are versioned as `ruddervirt.io/v1alpha1` — the `v1alpha1`
suffix is not decorative. The API can change without notice between releases.

## The four resources at a glance

| Kind | Short name | Purpose |
|---|---|---|
| [`VirtualMachineBuild`](#modules-virtualmachinebuild) | `vmb` | Boot, provision, and capture a VM (or set of VMs) as a golden template |
| [`VirtualMachineClone`](#clones-virtualmachineclone) | `vmc` | Instantiate a template into running VMs via CSI snapshot cloning |
| [`GradeRequest`](#grading-graderequest) | `gr` | Run commands over a VM's serial console and record the results |
| `VirtualMachineNamespace` | — | Internal bookkeeping; see [Namespace model](#namespace-model) |

Jump to: [Shared concepts](#shared-concepts) ·
[Modules](#modules-virtualmachinebuild) · [Clones](#clones-virtualmachineclone) ·
[Grading](#grading-graderequest) · [End-to-end walkthrough](#end-to-end-walkthrough)

---

## Shared concepts

### Resource naming & IDs

Builds and clones each get a generated ID once the controller first reconciles
them, and every resource they own is labeled and named from that ID:

- A build's ID is `status.buildID`; its namespace defaults to a generated
  `vm-{uid-hash}` (override with `spec.namespace`, or change the prefix with
  `spec.namespacePrefix`, default `"vm-"`).
- A clone's ID is `status.cloneID`; its namespace defaults to `ns-{uid-hash}`
  (override with `spec.namespace`/`spec.namespacePrefix`, default `"ns-"`).

All builds and clones share one Kubernetes namespace (`ruddervirt-system` by
default) — isolation between them is by label, not by Kubernetes namespace
boundary, unless you explicitly set `spec.namespace`.

### Namespace model

A build's namespace and a clone's namespace play different roles:

1. **Build namespace** — where the build VMs run while booting/provisioning.
2. On success, that *same* namespace becomes the **template namespace**
   (`status.templateNamespace == status.buildNamespace`): it now holds halted
   template VMs and their captured disks instead of running build VMs.
3. **Clone namespace** — a separate namespace created per `VirtualMachineClone`,
   holding the running, cloned VMs. It never overlaps with a build or template
   namespace.

Each build/clone's resources are also logically grouped by a
`VirtualMachineNamespace` resource — internal bookkeeping, not something you
typically author directly. It records `spec.ownerRef` (kind/name/namespace
of the owning Build or Clone), `status.phase` (`Active`/`Deleting`), and
`status.vms[]`. If your integration needs to enumerate active builds/clones
cluster-wide without watching both resource types separately, watching
`VirtualMachineNamespace` is a cheaper way to do it.

### Network model

Builds and clones share the same network types: a `network` block of
`vpcs[]` and `subnets[]`, and per-VM `nics[]` that reference a subnet by
name.

- **You usually don't need to declare a network at all.** Omit `network` and
  `nics` entirely and a VPC, subnet, and NIC are auto-created for you — this
  is the common single-VM, single-network case.
- Declare `network.vpcs[]` explicitly only when you need **multiple isolated
  VPCs** in one build (e.g. testing cross-VPC isolation) or need to name a
  subnet so multiple VMs can share it.
- `vpc.internet: true` enables NAT egress and public DNS (8.8.8.8, 1.1.1.1)
  for that VPC. Defaults to `false`.
- `subnet.cidr` is required; `subnet.dhcp` defaults to `true`.
- `subnet.unmanaged: true` turns the subnet into a bare L2 segment owned by a
  **guest gateway VM** (e.g. pfSense, or a Windows DC serving DHCP) instead of
  KubeOVN. Aileron still realizes it as an OVN logical switch (for cross-node
  L2 and VPC isolation), but disables OVN's own DHCP and relocates the
  mandatory OVN gateway port to the second-to-last usable IP so it can't
  collide with the guest's own gateway address. Requires a `/29` or wider
  CIDR. Because an unmanaged segment has no DHCP until the guest gateway VM
  itself is up, builds that need to provision *into* an unmanaged segment
  typically use [`buildOverrides`](#build-overrides) to run it as managed
  during the build only.
- `nics[].slot` (1-9) pins a NIC to a specific PCI slot so the same logical
  NIC keeps the same PCI address across a build, its layered children, and
  every clone. Without it, the guest sees "new" hardware whenever a
  downstream spec reorders the NIC list — and OS state that's bound to NIC
  identity (Windows adapter renames, static IPs, DHCP reservations by MAC,
  AD DNS bindings) ends up on the wrong interface. Pin `slot` on any NIC
  whose identity needs to survive layering or cloning.
- `nics[].model` defaults to `e1000` (broad in-box guest driver support,
  useful for Windows installs with no netkvm driver yet). Switch to `virtio`
  once the guest has the driver loaded, for higher throughput.

A build's template VMs carry their resolved network topology as an
annotation; clones read it to recreate equivalent VPCs/subnets under their
own ID unless `spec.network` overrides it.

### Labels & annotations reference

None of this is visible in the OpenAPI schema — it's the connective tissue
between resources, and the most useful thing an automation consumer can key
off of.

| Key | Meaning | Set on |
|---|---|---|
| `ruddervirt.io/build-id` | The owning build's `status.buildID` | All resources created by a build |
| `ruddervirt.io/build` | The `VirtualMachineBuild` name | Build resources |
| `ruddervirt.io/build-namespace` | The build's namespace | Build resources |
| `ruddervirt.io/vm` | The VM's short name (`spec.vms[].name` / `spec.templateName` VM name) | VMs, associated PVCs |
| `ruddervirt.io/clone` | The owning clone's `status.cloneID` | All resources created by a clone |
| `ruddervirt.io/component` | Resource role, e.g. `"template"`, `"clone"` | Build/clone-owned resources |
| `ruddervirt.io/os` | `"windows"` or `"linux"`, derived from the VM's provisioning shell type | Every VM (build, template, and clone) — this is what [grading](#grading-graderequest) reads to pick a console protocol |
| `ruddervirt.io/owner-kind`, `-name`, `-namespace` | Identifies the owning `VirtualMachineBuild`/`VirtualMachineClone` | `VirtualMachineNamespace` |
| `ruddervirt.io/origin` | Caller-supplied free-form attribution string, propagated verbatim from a Build/Clone onto its VMs; Aileron never interprets it | VMs, if set on the parent Build/Clone |
| `ruddervirt.io/age-anchor` | Effective creation time used for watchdog age checks (see [TTL & expiry](#ttl--expiry-model)) | Cloned VMs |
| `ruddervirt.io/expires-at` | Absolute RFC3339 timestamp a cloned VM becomes eligible for watchdog deletion | Cloned VMs |
| `ruddervirt.io/grade-request`, `-target-vm`, `-target-namespace` | Identify which `GradeRequest` and target VM a grader Job belongs to | Grader Jobs |
| `ruddervirt.io/invisible` | Set to `"true"` when the source `BuildVM`'s `invisible` is true; excludes the VM from the console/VNC UI | VMs, templates, and clones, when the source `spec.vms[].invisible` is `true` |

### Status contract

All four resources follow the same pattern:

- **`status.phase`** is a coarse state machine (see each resource's phase
  table below) — good for a progress indicator, but treat it as a hint, not a
  stable enum to branch deep logic on.
- **`status.conditions[]`** are standard Kubernetes `metav1.Condition`
  entries (`type`, `status`, `reason`, `message`, `lastTransitionTime`).
  Prefer checking conditions by `type` over string-matching a phase — new
  phases may be added over time, but a given condition `type` (e.g.
  `"Ready"`) is a more stable integration point.

---

## Modules (`VirtualMachineBuild`)

A module build boots one or more VMs, runs provisioners against them, shuts
them down, and captures their disks as a template. Clones later instantiate
that template.

### Minimal example

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: web-base
  namespace: ruddervirt-system
spec:
  vms:
    - name: builder
      source:
        url: "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
      resources:
        cpu: "1"
        memory: "2Gi"
      disks:
        - name: rootdisk
          size: "20Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      provisioners:
        - name: install-nginx
          type: shell
          shell:
            inline: |
              sudo apt-get update
              sudo apt-get install -y nginx
  timeout: 30m
```

No `network` or `nics` block is needed for the common case — a VPC, subnet,
and NIC are auto-created. `resources.cpu`/`resources.memory` are the only
required resource fields; quote fractional CPU values (e.g. `"0.5"`) since
YAML would otherwise parse them as a float.

### Anatomy of a build spec

**Source** (`spec.vms[].source`) — exactly one of:
- `url` — CDI imports a disk image from HTTP(S).
- `sourcePvc` — an existing PVC (`name`/`namespace`) with a base image.
- `containerDisk` — a container image reference holding a disk image.
- `buildRef` — the output of a prior, **`Succeeded`** `VirtualMachineBuild`
  (`name`, optional `namespace`, and `vmName` if the parent build has more
  than one VM). This is how [layered builds](#layered-builds) chain.
- `blank: true` — an empty disk, paired with `isos[]` for OS installs from
  install media.

**Resources & disks** — `resources.cpu`/`resources.memory` are required.
`disks[]` defaults to a single 20Gi virtio disk if omitted. The **first disk
is always the boot disk** and receives the source image; additional disks are
created blank (except under `buildRef`, where the boot disk is inherited from
the parent, so *every* listed disk is treated as an additional blank data
disk). `disks[].bus` defaults to `virtio`; keep the boot disk on a bootable
bus — `usb` is non-bootable, removable media, useful only for data/transfer
disks.

**Networking** — see [Network model](#network-model). Per-VM NIC assignments
live at `spec.vms[].nics[]`.

**Communicator & cloud-init** — `communicator.shell` (`bash` or
`powershell`, default `bash`) determines how files are uploaded and scripts
executed over SSH; set it to `powershell` for Windows guests. A cloud-init
disk is only attached when `cloudInit` is present at all — omit the field
entirely (not just leave it empty) to skip it, e.g. for ISO installs that use
`bootCommand`/preseed instead.

**Boot command** — `bootCommand[]` sends Packer-compatible VNC keystrokes
(`<enter>`, `<tab>`, `<wait20>`, etc.) before provisioning starts. Needed for
OS installs with no cloud-init path (see the [ISO install
example](#example-iso-install)); combine with `httpDirectory` and
`{{ .HTTPIP }}`/`{{ .HTTPPort }}` template variables to point an installer at
a preseed/answer file served from `spec.files`.

**Provisioners** — an ordered list, each running to completion before the
next starts:
- `shell` — **`inline` is a single multi-line script string** (`inline: |`),
  not a list of commands. Each shell provisioner runs one script; use
  multiple provisioner steps for multiple scripts, since variables and shell
  state do not carry across steps. `env` sets environment variables;
  `executeCommand` overrides the command wrapper (`{{ .Command }}`
  placeholder) — useful for e.g. `sudo -S` with a piped password.
- `file` — uploads a file from `source` (ConfigMap/Secret) or `fileRef`
  (references `spec.files[].name`) to `destination`.
- `reboot` — reboots and waits for SSH to drop then come back; `command`
  overrides the default (`sudo reboot` / `shutdown /r /f /t 0`).
- `windows-update` — runs the same search/filter/reboot loop as
  `packer-plugin-windows-update`; `searchCriteria` defaults to recommended
  updates, `filters[]` are PowerShell filter expressions
  (`"include:..."`/`"exclude:..."`), `updateLimit` caps updates per cycle
  (default 1000).
- `handbuild` — pauses the build for a human to interact with the VM over
  VNC; resumes on a "continue" signal. `instructions` is shown in the UI
  while paused.

**ISOs & floppy** — `isos[]` are cached as `ReadOnlyMany` PVCs keyed by
`checksum` (or the URL's hash if omitted) and shared across builds. `floppy`
attaches a disk built from named `spec.files[]` entries — useful for Windows
`Autounattend.xml`/sysprep or BIOS-era boot configuration.

**Invisible VMs** — `spec.vms[].invisible` (default `false`) excludes a VM
from the console/VNC-access UI, for the build itself and for every clone made
from its template. Use it for infrastructure VMs nobody should click into
(e.g. a webserver just serving a page, a DC/DNS box) — grading, networking,
and provisioning are unaffected. Clones cannot override it; a clone always
inherits invisibility from its template.

**Files** (`spec.files[]`) — named blobs (`inline` or `url`), referenced by
name from `httpDirectory`, `floppy`, or a `file` provisioner's `fileRef`. The
`name` doubles as the served filename/URL path, so use fully-qualified names
(e.g. `"Autounattend.xml"`, `"preseed.cfg"`).

<a id="build-overrides"></a>
**Build overrides** (`spec.buildOverrides`) — settings that apply **only**
during the build phase; the base spec values are what get captured into the
template and inherited by clones. Use this when the build phase legitimately
needs to differ from what clones should see:
- `vpcs[].internet` — give a VPC temporary internet access to install
  packages, without clones inheriting it.
- `subnets[].unmanaged` — force an `unmanaged` segment to be **managed**
  during the build (see [Network model](#network-model)), so provisioning has
  DHCP/relay reachability before the guest's own gateway exists; clones still
  get the unmanaged segment.
- `vms[].resources` / `vms[].nics` — extra CPU for compilation, or an
  extra provisioning-only NIC, without either leaking into the template. When
  a build-override NIC's `name` matches a base-spec NIC, its identity fields
  (`mac`/`slot`/`model`) are always taken from the base NIC — this is
  deliberate, since a build that ran with different NIC identity would bake
  guest state (e.g. a Windows DHCP reservation bound to a MAC) that no clone
  could ever match.

**Multi-VM builds** — `spec.vms[]` accepts more than one entry; all VMs boot
and provision in parallel and can reach each other by name over the shared
network.

<a id="layered-builds"></a>
**Layered builds** — chain builds with `source.buildRef`, referencing a
`Succeeded` parent by name (and `vmName` if it had multiple VMs). The parent
build's captured disk becomes this build's boot disk; provisioners in the
child build run against files/state left by the parent.

### Example: multi-VM build

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: server-client
  namespace: ruddervirt-system
spec:
  network:
    vpcs:
      - name: lab
        internet: false
    subnets:
      - name: lan
        vpc: lab
        cidr: "10.0.0.0/24"
  vms:
    - name: server
      source:
        url: "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
      resources:
        cpu: "1"
        memory: "1Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      nics:
        - name: lan0
          subnet: lan
          slot: 1
          ip: "10.0.0.10"
    - name: client
      source:
        url: "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
      resources:
        cpu: "1"
        memory: "1Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      nics:
        - name: lan0
          subnet: lan
          slot: 1
          ip: "10.0.0.11"
      provisioners:
        - name: verify-connectivity
          type: shell
          shell:
            inline: |
              ping -c 3 10.0.0.10
  timeout: 30m
```

### Example: layered build

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: web-base-with-app
  namespace: ruddervirt-system
spec:
  files:
    - name: app.conf
      inline: |
        listen 8080;
  vms:
    - name: builder
      source:
        buildRef:
          name: web-base
          vmName: builder
      resources:
        cpu: "1"
        memory: "2Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      provisioners:
        - name: upload-config
          type: file
          file:
            fileRef: app.conf
            destination: /etc/app/app.conf
        - name: restart-service
          type: shell
          shell:
            inline: |
              sudo systemctl restart app
  timeout: 30m
```

### Example: ISO install

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: debian-from-iso
  namespace: ruddervirt-system
spec:
  network:
    subnets:
      - name: lan
        cidr: "10.0.0.0/24"
  files:
    - name: preseed.cfg
      inline: |
        d-i auto-install/enable boolean true
        d-i passwd/user-fullname string skills
        d-i passwd/username string skills
        d-i passwd/user-password password skills
        d-i passwd/user-password-again password skills
        d-i preseed/late_command string \
          echo "%sudo ALL=(ALL:ALL) NOPASSWD:ALL" > /target/etc/sudoers.d/passwordless
  httpDirectory:
    files:
      - name: preseed.cfg
  vms:
    - name: builder
      source:
        blank: true
      resources:
        cpu: "2"
        memory: "4Gi"
      disks:
        - name: rootdisk
          size: "25Gi"
      isos:
        - url: "https://cdimage.debian.org/cdimage/archive/12.9.0/amd64/iso-cd/debian-12.9.0-amd64-netinst.iso"
      communicator:
        sshUsername: skills
        sshPassword: skills
        sshTimeout: 60m
      nics:
        - name: eth0
          subnet: lan
          slot: 1
          ip: "10.0.0.10"
      bootCommand:
        - "<wait20>"
        - "<down><down><enter>"
        - "<wait70>"
        - "http://{{ .HTTPIP }}:{{ .HTTPPort }}/preseed.cfg"
        - "<enter>"
  timeout: 90m
```

### Example: Windows handbuild

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: windows-manual-step
  namespace: ruddervirt-system
spec:
  vms:
    - name: builder
      source:
        url: "https://example.internal/images/windows-server-2022.qcow2"
      resources:
        cpu: "4"
        memory: "8Gi"
      communicator:
        shell: powershell
        sshUsername: Administrator
        sshPassword: "ChangeMe123!"
      provisioners:
        - name: manual-domain-join
          type: handbuild
          handbuild:
            instructions: "Join this VM to the lab domain via Settings > Accounts, then send the continue signal."
        - name: verify-join
          type: shell
          shell:
            inline: |
              Get-CimInstance Win32_ComputerSystem | Select-Object PartOfDomain
  timeout: 60m
```

### Example: network topology with an unmanaged segment

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: pfsense-lab
  namespace: ruddervirt-system
spec:
  network:
    vpcs:
      - name: lab
        internet: true
    subnets:
      - name: wan
        vpc: lab
        cidr: "10.0.0.0/24"
      - name: guest-lan
        vpc: lab
        cidr: "10.0.1.0/28"
        unmanaged: true   # pfSense will own DHCP/gateway on this segment
  buildOverrides:
    subnets:
      - name: guest-lan
        unmanaged: false  # managed during build so provisioning can reach it
  vms:
    - name: pfsense
      source:
        url: "https://example.internal/images/pfsense.qcow2"
      resources:
        cpu: "2"
        memory: "2Gi"
      nics:
        - name: wan0
          subnet: wan
          slot: 1
        - name: lan0
          subnet: guest-lan
          slot: 2
  timeout: 45m
```

### Example: build overrides for internet access

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: needs-packages
  namespace: ruddervirt-system
spec:
  network:
    vpcs:
      - name: lab
        internet: false   # clones get no internet
  buildOverrides:
    vpcs:
      - name: lab
        internet: true    # but the build does, to install packages
  vms:
    - name: builder
      source:
        url: "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
      resources:
        cpu: "1"
        memory: "1Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      provisioners:
        - name: install-packages
          type: shell
          shell:
            inline: |
              sudo apt-get update && sudo apt-get install -y nginx
  timeout: 30m
```

### Build lifecycle & status

`status.phase`:

| Phase | Meaning |
|---|---|
| `Pending` | Build accepted, not yet started |
| `Networking` | Creating VPCs/subnets |
| `Building` | VMs booting and provisioning |
| `CapturingDisks` | Provisioning finished; shutting down and snapshotting disks |
| `TemplateProvisioning` | Creating halted template VMs from captured disks |
| `Succeeded` | Template namespace ready — clones can reference this build's `status.buildID`-derived name |
| `Failed` | Build failed; see `status.message` and per-VM `status.vmStatuses[].message` |

Per-VM `status.vmStatuses[].phase` (`VMPhase`): `Pending` →
`SourceImporting` → `Booting` → `BootCommand` → `Provisioning` →
`ShuttingDown` → `DiskCaptured` → `Succeeded`/`Failed`. If a build hangs, this
is the first place to look — a stall in `Booting`/`BootCommand` usually means
the VM never came up or `bootCommand` keystrokes didn't match what the
console expected; check `status.vmStatuses[].provisionerResults[]` for
per-step outcomes once provisioning starts.

Conditions: `NetworkReady`, `AllVMsReady`, `DisksCaptured`,
`TemplateProvisioned`.

On success, `status.templateNamespace` (equal to `status.buildNamespace`) is
what a [`VirtualMachineClone`](#clones-virtualmachineclone)'s
`spec.templateName` resolves against (as `vm-{templateName}`).

### Common gotchas

- `shell.inline` is a **string**, not a YAML list — `inline: |` followed by
  a script, not `inline: ["cmd1", "cmd2"]`.
- Quote fractional CPU (`cpu: "0.5"`), and keep the boot disk on a bootable
  bus (`virtio` — not `usb`).
- Pin `nics[].slot` on any interface whose in-guest identity needs to
  survive a layered build or a clone.
- `source.buildRef` requires the parent build to already be `Succeeded` — it
  fails immediately otherwise.
- `cloudInit` is only attached when the field is present at all; an empty
  `{}` is enough, but omitting it entirely skips the cloud-init disk.

### Troubleshooting pointers

- `spec.timeout` (default `30m`) bounds the *entire* build; `spec.retries`
  (default `0`) controls automatic retry on failure.
- `spec.isoCacheTTL` (default 24h) controls how long imported ISO PVCs are
  kept before cleanup — increase it if repeated builds reuse the same ISO.
- `provisioners[].stepTimeout` bounds an individual step; unset, a step can
  run until the build-level timeout expires.

### See also

[Clones](#clones-virtualmachineclone) (what a `Succeeded` build feeds into) ·
[Shared concepts](#shared-concepts) · [End-to-end walkthrough](#end-to-end-walkthrough)

---

## Clones (`VirtualMachineClone`)

A clone instantiates a build's template into new, running VMs by taking CSI
volume snapshots of the template's disks and restoring them into fresh PVCs.

### Minimal example

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: web-base-lab-1
  namespace: ruddervirt-system
spec:
  templateName: web-base
  timeout: 15m
```

### Anatomy of a clone spec

**`templateName`** — resolves to the template namespace `vm-{templateName}`,
which must contain a `Succeeded` build's halted template VMs (see
[Namespace model](#namespace-model)).

**Namespace naming** — `spec.namespace` overrides the auto-generated clone
namespace; `spec.namespacePrefix` (default `"ns-"`) controls the generated
name's prefix (`ns-{uid-hash}`).

**Network overrides** — omit `spec.network` to inherit the template's
topology as-is (see [Network model](#network-model)); declaring it replaces
the topology for this clone only.

**`vmOverrides[]`** — per-VM customization; `name` must match a VM name that
exists in the template namespace. Supports overriding `nics` and `resources`.

**Egress gateway** — `spec.egressGateway.enabled` (default `true`) toggles
the `VpcEgressGateway` pod; setting it `false` scales the gateway to 0,
cutting internet access, without stopping the VM itself. VM power state is
managed separately (directly via KubeVirt).

<a id="ttl--expiry-model"></a>
**TTL & expiry model** — clones are expected to be temporary. Three fields
work together:
- `spec.ttl` — how long after the resolved age anchor the clone's VMs become
  eligible for deletion by a separate watchdog process. If unset, a
  cluster-configured default applies (`CLONE_DEFAULT_TTL` env var, 720h/30d
  built-in default). There is deliberately **no schema-level default** for
  this field — a default baked into the schema would materialize onto every
  clone at admission time and make the operator's env-configurable default
  unreachable.
- `spec.ageAnchor` — an explicit RFC3339 timestamp to use as the age anchor
  instead of this clone's own creation time.
- `spec.replacesCloneID` — names a prior clone (by its `status.cloneID`)
  whose age this clone should inherit. This is the "refresh without
  resetting the clock" pattern: if you tear down and recreate a clone (e.g.
  to pick up a new template), setting `replacesCloneID` to the predecessor's
  ID makes the new clone expire at the **same wall-clock time** the old one
  would have — `status.expiresAt` is inherited verbatim from the
  predecessor, not recomputed. Lookup by `replacesCloneID` takes priority
  over `spec.ageAnchor` when the predecessor still exists.

Reaching `expiresAt` marks a clone's VMs as *eligible* for deletion — it
doesn't delete them itself. A separate watchdog process acts on the
`ruddervirt.io/expires-at` annotation it stamps onto each VM.

### Example: clone with per-VM overrides

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: server-client-lab-1
  namespace: ruddervirt-system
spec:
  templateName: server-client
  vmOverrides:
    - name: client
      resources:
        cpu: "2"
        memory: "2Gi"
  timeout: 15m
```

### Example: TTL refresh via `replacesCloneID`

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: web-base-lab-1-v2
  namespace: ruddervirt-system
spec:
  templateName: web-base   # rebuilt/updated template
  replacesCloneID: "ns-abc123"   # status.cloneID of the clone this replaces
  timeout: 15m
```

### Example: network topology override

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: server-client-isolated
  namespace: ruddervirt-system
spec:
  templateName: server-client
  network:
    vpcs:
      - name: isolated
        internet: false
    subnets:
      - name: internal
        vpc: isolated
        cidr: "10.99.0.0/24"
  timeout: 15m
```

### Clone lifecycle & status

`status.phase`:

| Phase | Meaning |
|---|---|
| `Pending` | Clone accepted; resolving age anchor/expiry |
| `Validating` | Confirming the template exists and is well-formed |
| `SnapshotSelection` | Selecting the CSI VolumeSnapshot to restore from |
| `VolumeProvisioning` | Restoring snapshots into new PVCs |
| `Networking` | Creating VPCs/subnets/egress gateway |
| `VMProvisioning` | Creating the cloned VMs |
| `Ready` | All VMs running |
| `Failed` | See `status.message` |

Per-volume `status.volumeStates[].phase` (`CloneVolumePhase`): `Pending` →
`SnapshotSelected` → `SnapshotReady` → `PersistentVolumeReady` → `PVCBound`
→ `Complete`.

Conditions: `TemplateValidated`, `SnapshotSelected`, `SnapshotsReady`,
`VolumesReady`, `NetworkReady`, `VMProvisioned`, `Ready`,
`AgeAnchorResolved`, `ExpiryResolved`.

`status.ageAnchor` and `status.expiresAt` are resolved once, in the `Pending`
phase, and then held fixed — read them directly rather than recomputing TTL
math from `spec.ttl` and `metadata.creationTimestamp`, since a
`replacesCloneID` clone's `expiresAt` will not equal
`creationTimestamp + ttl`.

### Common gotchas

- `vmOverrides[].name` must exactly match a VM name present in the template
  namespace, or the override is inert.
- `egressGateway.enabled: false` cuts internet access but does **not** stop
  the VM — those are managed independently.
- Omitting `spec.network` inherits the template's topology wholesale;
  declaring any part of it replaces the topology for this clone, it does not
  merge field-by-field.

### Troubleshooting pointers

- `spec.timeout` defaults to `15m` for the whole clone operation.
- A clone stuck in `Validating` almost always means `spec.templateName`
  doesn't resolve to a `Succeeded` build's template namespace — check the
  build's `status.phase` first.

### See also

[Modules](#modules-virtualmachinebuild) (what produces a clonable template) ·
[Grading](#grading-graderequest) (what typically runs against a clone's VMs) ·
[Shared concepts](#shared-concepts)

---

## Grading (`GradeRequest`)

A `GradeRequest` runs a list of commands over a VM's **serial console**
(not SSH) and records the raw result of each. There is **no scoring or
rubric engine** — Aileron just returns `stdout`/`stderr`/`exitCode` per
command; the caller decides what "pass" means.

### Minimal example

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: GradeRequest
metadata:
  name: check-nginx
  namespace: ruddervirt-system
spec:
  namespace: ns-abc123
  vms:
    - name: web
      user: debian
      password: debian
      commands:
        - "systemctl is-active nginx"
```

### Anatomy of a grade spec

**`spec.namespace`** — the Kubernetes namespace containing the target VMs
(typically a clone's `status.cloneNamespace`).

**`spec.vms[]`** — each entry is `name` (the full KubeVirt VM name),
`commands[]` (run sequentially over the serial console), `user`/`password`
(serial console login), and optional `domain` for domain-joined Windows
guests authenticating via SAC.

**OS auto-detection** — the grading method (serial-Windows vs. serial-Linux)
is resolved by the controller from the target VM's `ruddervirt.io/os` label
(see [Labels & annotations](#labels--annotations-reference)). **Callers never
specify OS** — make sure the VM you're grading actually carries that label
(every VM Aileron creates does).

**Legacy fields** — `spec.vmName`/`spec.commands`/`spec.user`/
`spec.password`/`spec.domain` are still accepted for backward compatibility
and are normalized into a one-element `vms[]` internally. Use `vms[]`
directly for new integrations.

**Auto power-on/off** — if a target VM is powered off, the controller powers
it on automatically before grading (`status.vmStatuses[].autoStarted` /
`bootStartedAt` record this) and waits `grading.bootWaitSeconds` (Helm value,
default 240s) before running commands. Only VMs the controller itself
started are powered back off afterward (`poweredOff`); VMs that were already
running are left running.

**Concurrency queue** — grading is capped cluster-wide
(`grading.maxConcurrent` Helm value, default 10; a slot covers both
booting-for-grade and the running grade job). Each VM in `spec.vms[]` gets
its **own grader Job** and competes independently for a free slot — VMs in
the same `GradeRequest` are not batched or guaranteed to start together.
`status.activeSlots`/`maxSlots`/`queuedCount` and per-VM
`status.vmStatuses[].queuePosition` (1-based, cleared once admitted) are
surfaced so a caller can render queue state without recomputing it.

### Example: Windows, domain-joined

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: GradeRequest
metadata:
  name: check-domain-join
  namespace: ruddervirt-system
spec:
  namespace: ns-def456
  vms:
    - name: workstation
      user: labuser
      password: "ChangeMe123!"
      domain: LAB
      commands:
        - "whoami"
        - "Get-CimInstance Win32_ComputerSystem | Select-Object PartOfDomain"
```

### Example: multi-VM grading

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: GradeRequest
metadata:
  name: check-server-client
  namespace: ruddervirt-system
spec:
  namespace: ns-ghi789
  vms:
    - name: server
      user: debian
      password: debian
      commands:
        - "systemctl is-active nginx"
    - name: client
      user: debian
      password: debian
      commands:
        - "curl -sf http://10.0.0.10 >/dev/null && echo reachable"
```

### Grade lifecycle & status

`status.phase` (`GradeRequestPhase`): `Pending` → `Running` → `Ready` /
`Failed`. Per-VM `status.vmStatuses[].phase` follows the same enum
independently, since each VM's grading job runs on its own schedule.

`status.vmStatuses[].results[]` mirrors `spec.vms[].commands[]` positionally:
each result has `stdout`, `stderr`, and `exitCode`. The established
convention is to make each command **self-checking** — exit `0` for pass,
non-zero for fail — since there's no separate scoring step to interpret
output for you.

### Common gotchas

- Commands are opaque to Aileron; there's no rubric or partial-credit engine.
  Design each command to be self-checking.
- `domain` only makes sense for domain-joined Windows guests using SAC; leave
  it unset otherwise.
- Never set an OS/grading-method field yourself — it doesn't exist in the
  spec. Make sure the target VM's `ruddervirt.io/os` label is correct instead.

### Troubleshooting pointers

- A VM stuck with a `queuePosition` set means it's waiting on a global
  concurrency slot (`grading.maxConcurrent`) — check `status.activeSlots`
  against `status.maxSlots` across the cluster, not just this request.
- If `status.vmStatuses[].phase` is `Failed` with no `results[]`, check
  `status.vmStatuses[].message` — this usually means the serial console
  login itself failed (wrong `user`/`password`, or the VM never finished
  booting within the boot-wait grace period), not that a command failed.

### See also

[Clones](#clones-virtualmachineclone) (what you typically grade) ·
[Shared concepts](#shared-concepts) · [End-to-end walkthrough](#end-to-end-walkthrough)

---

## End-to-end walkthrough

A typical integration chains all three resources: build once, clone many
times, grade each clone.

**1. Build a template once.**

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: web-base
  namespace: ruddervirt-system
spec:
  vms:
    - name: web
      source:
        url: "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
      resources:
        cpu: "1"
        memory: "2Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      provisioners:
        - name: install-nginx
          type: shell
          shell:
            inline: |
              sudo apt-get update && sudo apt-get install -y nginx
  timeout: 30m
```

Watch `status.phase` until it reaches `Succeeded`. The build's `metadata.name`
(`web-base`) is now usable as a clone's `templateName` — no need to read
`status.buildID` for this step, since `templateName` resolves by the build's
*name*, not its generated ID.

**2. Clone it for a lab instance.**

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: web-base-lab-42
  namespace: ruddervirt-system
spec:
  templateName: web-base
  ttl: 4h
  timeout: 15m
```

Watch `status.phase` until it reaches `Ready`, then read
`status.cloneNamespace` and `status.vmStatuses[].name` — the VM name here
(`web`, matching the build's VM name unless overridden) is what a grade
request targets next.

**3. Grade the running clone.**

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: GradeRequest
metadata:
  name: web-base-lab-42-check
  namespace: ruddervirt-system
spec:
  namespace: ns-<clone's status.cloneNamespace>
  vms:
    - name: web
      user: debian
      password: debian
      commands:
        - "systemctl is-active nginx"
```

Watch `status.phase` until `Ready`, then read
`status.vmStatuses[0].results[0].exitCode` — `0` means nginx was active.

When the clone's `ttl` (4h here) elapses from creation, the watchdog becomes
eligible to delete `web`. To refresh the lab without resetting that clock —
e.g. rolling forward to a newer `web-base` build without extending how long
the student's total session can run — create a new clone with
`replacesCloneID` set to the old clone's `status.cloneID`.

