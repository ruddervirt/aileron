# Aileron End-User Documentation

This guide covers the creation of resources with Aileron, checking their status, and understanding their connections using labels and naming conventions. It's meant for users writing YAML programmatically, such as fleet-management controllers, UI backends, or operators.

**Aileron installs a schema to validate fields** (required/optional fields, types, enums, defaults). With Kubernetes access, `kubectl explain <kind>.spec` provides this information. UI or API integrations should offer similar validation. This guide also covers workflow, resource relationships, label/annotation conventions, and status/phase semantics.

Resources are versioned as `ruddervirt.io/v1alpha1`. The `v1alpha1` suffix indicates potential API changes between releases.

## The Four Resources at a Glance

| Kind | Short name | Purpose |
|---|---|---|
| [`VirtualMachineBuild`](#modules-virtualmachinebuild) | `vmb` | Create, provision, and capture a VM or VMs as a template |
| [`VirtualMachineClone`](#clones-virtualmachineclone) | `vmc` | Create running VMs from a template via CSI snapshot cloning |
| [`GradeRequest`](#grading-graderequest) | `gr` | Execute commands on a VM's serial console and log the results |
| `VirtualMachineNamespace` | — | Internal use; see [Namespace model](#namespace-model) |

Jump to: [Shared concepts](#shared-concepts) · [Modules](#modules-virtualmachinebuild) · [Clones](#clones-virtualmachineclone) · [Grading](#grading-graderequest) · [End-to-end walkthrough](#end-to-end-walkthrough)

---

## Shared Concepts

### Resource Naming & IDs

Builds and clones get an ID when first processed, and resources they own are labeled and named with that ID:

- A build's ID is `status.buildID`; its namespace defaults to `vm-{uid-hash}` (can be changed with `spec.namespace` or `spec.namespacePrefix`, default `"vm-"`).
- A clone's ID is `status.cloneID`; its namespace defaults to `ns-{uid-hash}` (can be changed with `spec.namespace`/`spec.namespacePrefix`, default `"ns-"`).

All builds and clones share a single Kubernetes namespace (`ruddervirt-system` by default) — they are isolated by label, unless `spec.namespace` is explicitly set.

### Namespace Model

Build and clone namespaces have different functions:

1. **Build namespace** — where build VMs operate during boot/provisioning.
2. On success, becomes the **template namespace** (`status.templateNamespace == status.buildNamespace`), holding the halted template VMs and disks.
3. **Clone namespace** — separate namespace for each `VirtualMachineClone`, containing running cloned VMs. Does not overlap with a build or template namespace.

Each build or clone's resources are grouped by a `VirtualMachineNamespace` resource — used for internal tracking, not direct authoring. It records `spec.ownerRef` (kind/name/namespace of the owning Build or Clone), `status.phase` (`Active`/`Deleting`), and `status.vms[]`. To list active builds/clones cluster-wide without watching both resource types, monitor `VirtualMachineNamespace`.

### Network Model

Builds and clones share network types: a `network` block of `vpcs[]` and `subnets[]`, with per-VM `nics[]` referring to a subnet by name.

- **Usually, no network declaration is needed.** Omit `network` and `nics`, and a VPC, subnet, and NIC are auto-created — typical for a single-VM, single-network setup.
- Declare `network.vpcs[]` only when multiple isolated VPCs are needed in one build or to name a subnet for sharing among multiple VMs.
- `vpc.internet: true` enables NAT egress and public DNS. Defaults to `false`.
- `subnet.cidr` is required; `subnet.dhcp` defaults to `true`.
- `subnet.unmanaged: true` changes the subnet to a L2 segment owned by a **guest gateway VM** (like pfSense or Windows DC for DHCP) instead of KubeOVN. Aileron still uses it as an OVN logical switch but disables OVN's own DHCP and relocates the OVN gateway port. Requires a `/29` or wider CIDR. For unmanaged segments needing DHCP during build, use [`buildOverrides`](#build-overrides) to manage it temporarily.
- `nics[].slot` (1-9) assigns a NIC to a PCI slot, keeping the same address across builds and clones. Without it, reordering NICs in downstream specs can confuse OS state tied to NIC identity.
- `nics[].model` defaults to `e1000`; switch to `virtio` after the guest loads the driver for better performance.

A build's template VMs carry resolved network topology as an annotation; clones recreate equivalent VPCs/subnets unless `spec.network` overrides it.

### Labels & Annotations Reference

This isn't in the OpenAPI schema — it's the linkage between resources, essential for automation.

| Key | Meaning | Set on |
|---|---|---|
| `ruddervirt.io/build-id` | The build's `status.buildID` | Resources from a build |
| `ruddervirt.io/build` | The `VirtualMachineBuild` name | Build resources |
| `ruddervirt.io/build-namespace` | The build's namespace | Build resources |
| `ruddervirt.io/vm` | The VM's short name (`spec.vms[].name` / `spec.templateName`) | VMs, associated PVCs |
| `ruddervirt.io/clone` | The clone's `status.cloneID` | Resources from a clone |
| `ruddervirt.io/component` | Resource role, like `"template"`, `"clone"` | Resources from builds/clones |
| `ruddervirt.io/os` | `"windows"` or `"linux"`, based on the VM's provisioning shell type | Every VM — read by [grading](#grading-graderequest) for console protocol selection |
| `ruddervirt.io/owner-kind`, `-name`, `-namespace` | The owning `VirtualMachineBuild`/`VirtualMachineClone` | `VirtualMachineNamespace` |
| `ruddervirt.io/origin` | Free-form attribution string from a Build/Clone; not interpreted by Aileron | VMs, if set on the parent Build/Clone |
| `ruddervirt.io/age-anchor` | Creation time for watchdog age checks (see [TTL & expiry](#ttl--expiry-model)) | Cloned VMs |
| `ruddervirt.io/expires-at` | RFC3339 timestamp when a cloned VM can be deleted by watchdog | Cloned VMs |
| `ruddervirt.io/grade-request`, `-target-vm`, `-target-namespace` | Identify `GradeRequest` and target VM for a grader Job | Grader Jobs |
| `ruddervirt.io/invisible` | Set to `"true"` for `invisible` `BuildVM`s; hides VM from console/VNC UI | VMs, templates, and clones, when the source `spec.vms[].invisible` is `true` |

### Status Contract

All resources follow a similar pattern:

- **`status.phase`** is a broad status indicator — useful for progress, but not a stable enum for deep logic.
- **`status.conditions[]`** are Kubernetes `metav1.Condition` entries. Check conditions by `type` rather than phase string-matching, as phases may change over time, but a condition `type` (like `"Ready"`) is more stable.

---

## Modules (`VirtualMachineBuild`)

A module build boots VMs, provisions them, shuts them down, and captures their disks as a template for cloning.

### Minimal Example

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

For common cases, omit `network` or `nics`. Auto-creation covers a VPC, subnet, and NIC. `resources.cpu`/`resources.memory` are mandatory; quote fractional CPU values (`"0.5"`) to avoid YAML parsing as floats.

### Anatomy of a Build Spec

**Source** (`spec.vms[].source`) — one of these options:
- `url` — CDI imports a disk image from HTTP(S).
- `sourcePvc` — an existing PVC (`name`/`namespace`) with a base image.
- `containerDisk` — a container image reference with a disk image.
- `buildRef` — output of a successful `VirtualMachineBuild` (`name`, `namespace`, and optionally `vmName` if multiple VMs exist). Used for [layered builds](#layered-builds).
- `blank: true` — an empty disk, combined with `isos[]` for installs from media.

**Resources & Disks** — `resources.cpu`/`resources.memory` are required. `disks[]` defaults to a single 20Gi virtio disk if not specified. The **first disk is the boot disk** and receives the source image; extra disks are blank (under `buildRef`, the boot disk is inherited, so all listed disks are new blank data disks). `disks[].bus` defaults to `virtio`; boot disks need a bootable bus — `usb` is for data/transfer only.

**Networking** — see [Network model](#network-model). Per-VM NICs are in `spec.vms[].nics[]`.

**Communicator & Cloud-Init** — `communicator.shell` (`bash` or `powershell`, default `bash`) decides how files/scripts run over SSH; use `powershell` for Windows. A cloud-init disk is only attached if `cloudInit` is present — omit it entirely for setups like ISO installs that use `bootCommand`/preseed.

**Boot Command** — `bootCommand[]` sends Packer-compatible VNC keystrokes (`<enter>`, `<tab>`, etc.) before provisioning. Needed for OS installs without cloud-init. Use with `httpDirectory` and `{{ .HTTPIP }}`/`{{ .HTTPPort }}` variables for pointing to preseed/answer files served from `spec.files`.

**Provisioners** — an ordered list, each completes before the next starts:
- `shell` — **`inline` is a single script string** (`inline: |`), not command list. Each shell provisioner runs one script; use multiple provisioners for multiple scripts, as variables and state don't carry over. `env` sets environment vars; `executeCommand` changes the command wrapper (useful for `sudo -S` with a piped password).
- `file` — uploads a file from `source` (ConfigMap/Secret) or `fileRef` (references `spec.files[].name`) to `destination`.
- `reboot` — reboots and waits for SSH to drop and return; `command` changes the default (`sudo reboot`/`shutdown /r /f /t 0`). This provisioner the only safe way to restart a virtual machine during a build. You may be able to reboot using standard shell commands, but you might experience a race condition from the next provisioner (where the next provisioner would start quickly and then the machine would reboot).
- `windows-update` — runs a search/filter/reboot loop like `packer-plugin-windows-update`; `searchCriteria` defaults to recommended updates, `filters[]` are PowerShell expressions (`"include:..."`/`"exclude:..."`), `updateLimit` caps updates per cycle (default 1000).
- `handbuild` — pauses the build for manual VM interaction over VNC; resumes on a "continue" signal. `instructions` are shown in the UI while paused.

**ISOs & Floppy** — `isos[]` are cached `ReadOnlyMany` PVCs keyed by `checksum` (or URL hash if omitted) and shared across builds. `floppy` attaches a disk from named `spec.files[]` entries — useful for Windows `Autounattend.xml`/sysprep or BIOS boot config. **Build-only**: not attached to cloned VMs.

**Invisible VMs** — `spec.vms[].invisible` (default `false`) hides a VM from the console/UI but doesn't affect grading or networking. Clones inherit invisibility; they can't change it.

**Files** (`spec.files[]`) — named blobs (`inline` or `url`) referenced by `httpDirectory`, `floppy`, or a `file` provisioner's `fileRef`. `name` serves as filename/URL path, so use specific names (e.g. `"Autounattend.xml"`, `"preseed.cfg"`).

<a id="build-overrides"></a>
**Build Overrides** (`spec.buildOverrides`) — settings for the build phase only; base spec values go into the template and are used by clones. Useful when build needs differ from what clones require:
- `vpcs[].internet` — temporary internet access for package installs, not inherited by clones.
- `subnets[].unmanaged` — force an `unmanaged` segment to be **managed** during the build (see [Network model](#network-model)), so DHCP/relay is reachable before the guest's gateway exists; clones get the unmanaged segment.
- `vms[].resources` / `vms[].nics` — extra build CPU or a NIC without affecting the template. If a build-override NIC matches a base-spec NIC by `name`, identity fields (`mac`/`slot`/`model`) are taken from the base NIC to avoid guest state issues.

**Multi-VM Builds** — `spec.vms[]` allows multiple entries; all VMs boot and provision in parallel and can communicate over the shared network.

<a id="layered-builds"></a>
**Layered Builds** — chain builds with `source.buildRef`, referencing a `Succeeded` parent by name (and `vmName` if multiple VMs exist). The parent's captured disk becomes the boot disk; child build provisioners run against the parent's state.

### Example: Multi-VM Build

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

### Example: Layered Build

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

### Example: ISO Install

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

### Example: Windows Handbuild

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

### Example: Network Topology with an Unmanaged Segment

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

### Example: Build Overrides for Internet Access

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

### Build Lifecycle & Status

`status.phase`:

| Phase | Meaning |
|---|---|
| `Pending` | Build accepted, not started |
| `Networking` | Creating VPCs/subnets |
| `Building` | VMs booting and provisioning |
| `CapturingDisks` | Provisioning done; shutting down and snapshotting disks |
| `TemplateProvisioning` | Creating halted template VMs from disks |
| `Succeeded` | Template namespace ready — clones can use this build's `status.buildID`-derived name |
| `Failed` | Build failed; check `status.message` and per-VM `status.vmStatuses[].message` |

Per-VM `status.vmStatuses[].phase` (`VMPhase`): `Pending` → `SourceImporting` → `Booting` → `BootCommand` → `Provisioning` → `ShuttingDown` → `DiskCaptured` → `Succeeded`/`Failed`. For a stalled build, check here first — `Booting`/`BootCommand` may indicate the VM didn't come up or `bootCommand` mismatched console expectations; check `status.vmStatuses[].provisionerResults[]` for step outcomes after provisioning starts.

Conditions: `NetworkReady`, `AllVMsReady`, `DisksCaptured`, `TemplateProvisioned`.

On success, `status.templateNamespace` (equal to `status.buildNamespace`) is what a [`VirtualMachineClone`](#clones-virtualmachineclone)'s `spec.templateName` resolves against (as `vm-{templateName}`).

### Common Gotchas

- `shell.inline` is a **string**, not a YAML list — use `inline: |` followed by a script, not `inline: ["cmd1", "cmd2"]`.
- Quote fractional CPU (`cpu: "0.5"`), and keep the boot disk on a bootable bus (`virtio`, not `usb`).
- Pin `nics[].slot` for any interface whose in-guest identity must survive a layered build or clone.
- `source.buildRef` requires the parent build to be `Succeeded` — it fails immediately otherwise.
- `cloudInit` is attached only if the field is present; an empty `{}` works, but omit it entirely to skip the cloud-init disk.
- Never trigger a reboot from inside a `shell` provisioner (`Restart-Computer`, `shutdown.exe`, `sudo reboot`, etc.) — the coordinator moves to the next provisioner as soon as the script exits, and it will race the reboot and fail with a dropped connection (e.g. `SFTP ... unexpected EOF`) or a Task Scheduler termination code. Use a dedicated `reboot` provisioner step, which waits for SSH to drop and come back before continuing.

### Troubleshooting Pointers

- `spec.timeout` (default `30m`) limits the entire build; `spec.retries` (default `0`) manages automatic retries on failure.
- `spec.isoCacheTTL` (default 24h) determines how long ISO PVCs are kept before cleanup — increase if repeated builds reuse the same ISO.
- `provisioners[].stepTimeout` limits an individual step; if unset, a step can run until the build-level timeout expires.

### See Also

[Clones](#clones-virtualmachineclone) (what a `Succeeded` build feeds into) · [Shared concepts](#shared-concepts) · [End-to-end walkthrough](#end-to-end-walkthrough)

---

## Clones (`VirtualMachineClone`)

A clone turns a build's template into running VMs by taking CSI volume snapshots of the template's disks and restoring them as new PVCs.

### Minimal Example

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

### Anatomy of a Clone Spec

**`templateName`** — resolves to the template namespace `vm-{templateName}`, which must contain `Succeeded` build's halted template VMs (see [Namespace model](#namespace-model)).

**Namespace Naming** — `spec.namespace` overrides the auto-generated clone namespace; `spec.namespacePrefix` (default `"ns-"`) affects the generated name's prefix (`ns-{uid-hash}`).

**Network Overrides** — omit `spec.network` to inherit the template's topology; declaring it changes the topology for this clone only.

**`vmOverrides[]`** — per-VM customization; `name` must match an existing VM in the template namespace. Supports overriding `nics` and `resources`.

**Egress Gateway** — `spec.egressGateway.enabled` (default `true`) controls the `VpcEgressGateway` pod; `false` scales the gateway to 0, cutting internet access, without affecting VM power state. VM power is managed separately (via KubeVirt).

<a id="ttl--expiry-model"></a>
**TTL & Expiry Model** — clones are typically temporary. Three fields work together:
- `spec.ttl` — after the resolved age anchor, the clone's VMs become eligible for deletion by a watchdog process. If unset, a default applies (`CLONE_DEFAULT_TTL` env var, 720h/30d built-in default). There is no schema-level default — a schema default would materialize on each clone at admission, making the operator's configurable default unreachable.
- `spec.ageAnchor` — an RFC3339 timestamp to use instead of this clone's creation time.
- `spec.replacesCloneID` — names a prior clone (by `status.cloneID`) whose age this clone inherits. This "refresh without resetting the clock" pattern lets a replacement clone expire at the same time as its predecessor — `status.expiresAt` is inherited, not recomputed. Lookup by `replacesCloneID` takes priority over `spec.ageAnchor` if the predecessor exists.

Reaching `expiresAt` marks a clone's VMs as *eligible* for deletion — it doesn't perform deletion. A separate watchdog process uses the `ruddervirt.io/expires-at` annotation on each VM.

### Example: Clone with Per-VM Overrides

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

### Example: TTL Refresh via `replacesCloneID`

```yaml
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: web-base-lab-1-v2
  namespace: ruddervirt-system
spec:
  templateName: web-base   # updated template
  replacesCloneID: "ns-abc123"   # status.cloneID of the clone this replaces
  timeout: 15m
```

### Example: Network Topology Override

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

### Clone Lifecycle & Status

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

Per-volume `status.volumeStates[].phase` (`CloneVolumePhase`): `Pending` → `SnapshotSelected` → `SnapshotReady` → `PersistentVolumeReady` → `PVCBound` → `Complete`.

Conditions: `TemplateValidated`, `SnapshotSelected`, `SnapshotsReady`, `VolumesReady`, `NetworkReady`, `VMProvisioned`, `Ready`, `AgeAnchorResolved`, `ExpiryResolved`.

`status.ageAnchor` and `status.expiresAt` are resolved once in the `Pending` phase and then remain fixed — read them directly rather than recomputing TTL math from `spec.ttl` and `metadata.creationTimestamp`, as a `replacesCloneID` clone's `expiresAt` won't equal `creationTimestamp + ttl`.

### Common Gotchas

- `vmOverrides[].name` must match a VM name in the template namespace, or the override won't apply.
- `egressGateway.enabled: false` stops internet access but not the VM itself.
- Omitting `spec.network` uses the template's topology; declaring it changes the topology for this clone.

### Troubleshooting Pointers

- `spec.timeout` defaults to `15m` for the whole clone process.
- A clone stuck in `Validating` usually means `spec.templateName` doesn't resolve to a `Succeeded` build's template namespace — check the build's `status.phase` first.

### See Also

[Modules](#modules-virtualmachinebuild) (what produces a clonable template) · [Grading](#grading-graderequest) (what typically runs against a clone's VMs) · [Shared concepts](#shared-concepts)

---

## Grading (`GradeRequest`)

A `GradeRequest` runs commands over a VM's **serial console** (not SSH) and logs the raw result of each. There is **no scoring or rubric engine** — Aileron just returns `stdout`/`stderr`/`exitCode` per command; the caller determines what "pass" means.

### Minimal Example

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

### Anatomy of a Grade Spec

**`spec.namespace`** — the Kubernetes namespace containing the target VMs (usually a clone's `status.cloneNamespace`).

**`spec.vms[]`** — each entry includes `name` (full KubeVirt VM name), `commands[]` (run sequentially over the serial console), `user`/`password` (serial console login), and optional `domain` for domain-joined Windows guests using SAC.

**OS Auto-Detection** — the grading method (serial-Windows vs. serial-Linux) is determined by the controller from the target VM's `ruddervirt.io/os` label (see [Labels & annotations](#labels--annotations-reference)). **Don't specify OS** — ensure the VM being graded has the correct label (all VMs created by Aileron do).

**Legacy Fields** — `spec.vmName`/`spec.commands`/`spec.user`/ `spec.password`/`spec.domain` are accepted for backward compatibility and are converted into a single-element `vms[]`. Use `vms[]` directly for new integrations.

**Auto Power-On/Off** — if a target VM is off, the controller powers it on before grading (`status.vmStatuses[].autoStarted` / `bootStartedAt` record this) and waits `grading.bootWaitSeconds` (Helm value, default 240s) before running commands. Only VMs auto-started by the controller are powered off afterward (`poweredOff`); others stay running.

**Concurrency Queue** — grading is capped cluster-wide (`grading.maxConcurrent` Helm value, default 10; a slot covers booting-for-grade and the running grade job). Each VM in `spec.vms[]` gets its own grader Job and competes for a slot — VMs in the same `GradeRequest` aren't batched to start together. `status.activeSlots`/`maxSlots`/`queuedCount` and per-VM `status.vmStatuses[].queuePosition` (1-based, cleared once admitted) show queue state without recomputation.

### Example: Windows, Domain-Joined

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

### Example: Multi-VM Grading

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

### Grade Lifecycle & Status

`status.phase` (`GradeRequestPhase`): `Pending` → `Running` → `Ready` / `Failed`. Per-VM `status.vmStatuses[].phase` follows the same pattern independently, as each VM's grading job runs on its own.

`status.vmStatuses[].results[]` mirrors `spec.vms[].commands[]` positionally: each result has `stdout`, `stderr`, and `exitCode`. The convention is for each command to be **self-checking** — exit `0` for pass, non-zero for fail — since there's no separate scoring process.

### Common Gotchas

- Commands are opaque to Aileron; there's no scoring engine. Each command should be self-checking.
- `domain` is only relevant for domain-joined Windows guests using SAC; leave it unset otherwise.
- Don't specify an OS/grading-method field — it doesn't exist in the spec. Ensure the target VM's `ruddervirt.io/os` label is correct.

### Troubleshooting Pointers

- A VM stuck with a `queuePosition` means it's waiting for a global concurrency slot (`grading.maxConcurrent`) — check `status.activeSlots` against `status.maxSlots` cluster-wide, not just this request.
- If `status.vmStatuses[].phase` is `Failed` without `results[]`, check `status.vmStatuses[].message` — this usually indicates a login failure (wrong `user`/`password`, or the VM didn't boot within the grace period), not a command failure.

### See Also

[Clones](#clones-virtualmachineclone) (what you typically grade) · [Shared concepts](#shared-concepts) · [End-to-end walkthrough](#end-to-end-walkthrough)

---

## End-to-End Walkthrough

A typical integration uses all three resources: build once, clone multiple times, grade each clone.

**1. Build a Template Once.**

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

Watch `status.phase` until `Succeeded`. The build's `metadata.name` (`web-base`) is now usable as a clone's `templateName` — no need to read `status.buildID` for this step, since `templateName` resolves by the build's *name*, not its generated ID.

**2. Clone It for a Lab Instance.**

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

Watch `status.phase` until `Ready`, then read `status.cloneNamespace` and `status.vmStatuses[].name` — the VM name here (`web`, matching the build's VM name unless overridden) is what a grade request targets next.

**3. Grade the Running Clone.**

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

Watch `status.phase` until `Ready`, then read `status.vmStatuses[0].results[0].exitCode` — `0` means nginx was active.

When the clone's `ttl` (4h here) elapses from creation, the watchdog becomes eligible to delete `web`. To refresh the lab without resetting that clock — e.g., rolling forward to a newer `web-base` build without extending the session — create a new clone with `replacesCloneID` set to the old clone's `status.cloneID`.
