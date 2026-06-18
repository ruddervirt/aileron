# Aileron

> **Looking for a quick start?** You probably want to install our operating system, which comes bundled with everything — see [ruddervirt-os](https://github.com/ruddervirt/ruddervirt-os).

A Kubernetes operator for building repeatable "modules" of VMs. Aileron automates the full lifecycle: boot a VM from a cloud image or ISO, run provisioners (shell scripts, file uploads), capture the disk as a template, and clone it into running instances with isolated networking.

## Rudder Virt

[Rudder Virt](https://ruddervirt.com) uses Aileron to run all of its virtual machines. Running Aileron on its own is not enough to connect your server to Rudder Virt — there is additional setup to do. Please contact [selfhosted@ruddervirt.com](mailto:selfhosted@ruddervirt.com) for more information.

## Features

- **Multi-VM builds** — boot multiple VMs in parallel with isolated networking
- **Layered VM builds** — chain builds together, each layer adding to the previous
- **Packer-like provisioners** — automate VM builds with shell scripts and file uploads
- **Snapshot cloning** — CSI volume snapshots for fast, space-efficient clones
- **Network isolation** — each build/clone gets its own KubeOVN VPC, subnets, and egress
- **Self-hosted web UI** — submit builds/clones, watch status, and open consoles
- **Grading** — run commands over a VM's serial console and capture results via the `GradeRequest` CRD

## Prerequisites

- Kubernetes 1.30+ (tested on k3s 1.33)
- [KubeVirt](https://kubevirt.io/) 1.4+
- [CDI](https://github.com/kubevirt/containerized-data-importer) (Containerized Data Importer)
- [KubeOVN](https://kubeovn.github.io/docs/) for network isolation
- [Rook-Ceph](https://rook.io/) or another CSI driver with snapshot support (for cloning)

## How It Works

### Building a module

1. The operator creates a KubeOVN VPC and subnet for the build
2. KubeVirt VMs are created
3. VMs boot with cloud-init, or via boot commands typed over VNC
4. Shell and file provisioners run over SSH through a relay pod
5. VMs are shut down and their disks are cloned to output DataVolumes
6. Halted template VMs are created referencing the output disks

### Cloning

1. The operator validates the template build exists
2. CSI volume snapshots are taken of the template disks
3. New PVCs are created from the snapshots
4. A new KubeOVN VPC and subnet are created (derived from the template's topology)
5. Running VMs are created with the cloned disks and new network

## Developing

```bash
make build         # Build the Docker images locally
make push          # Build + push images to the registry
make helm-publish CHART_VERSION=1.2.3   # Package + push the versioned Helm chart
```

Deployment is owned by the consuming environments, not this repo — they pull the
published images and Helm chart. 

## Configuration

### Helm Values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/ruddervirt/aileron` | Operator image |
| `image.tag` | `""` | Image tag (empty inherits `.Chart.AppVersion`; override locally with `--set image.tag=<sha>`) |
| `debug` | `false` | Retain finished Jobs and failed build resources for inspection |
| `failureRetention` | `30m` | How long to keep failed build resources before cleanup (Go duration; ignored when `debug=true`) |
| `vmResources.cpu` | `2` | CPU request per build/clone VM (controls scheduling concurrency) |
| `vmResources.memory` | `4096Mi` | Memory request per build/clone VM |
| `buildLimits.maxCPU` | `8` | Max CPU cores per VM, clamped at admission (0 = unlimited) |
| `buildLimits.maxMemory` | `16Gi` | Max memory per VM, clamped at admission (empty = unlimited) |
| `buildLimits.maxDiskSize` | `50Gi` | Max size per disk, clamped at admission (empty = unlimited) |
| `buildLimits.maxDiskCount` | `3` | Max disks per VM; exceeding fails the build (0 = unlimited) |
| `buildLimits.maxVMCount` | `4` | Max VMs per build; exceeding fails the build (0 = unlimited) |
| `egressExternal.enabled` | `true` | Enable KubeOVN egress for internet access |
| `egressExternal.cidr` | `172.17.0.0/16` | Egress subnet CIDR |
| `egressExternal.gateway` | `172.17.0.1` | Egress gateway IP |
| `grading.enabled` | `true` | Enable the `GradeRequest` reconciler |
| `grading.bootWaitSeconds` | `240` | Seconds to wait after powering on a stopped VM before grading |
| `aileronUI.enabled` | `true` | Deploy the self-hosted web UI (unauthenticated — trusted networks only) |
| `aileronUI.service.nodePort` | `30806` | NodePort for the UI (empty = auto-assign) |
| `vncGateway.enabled` | `true` | Deploy the open-source VNC console gateway |
| `vncGateway.port` | `7778` | Cluster-internal gateway listener port |

## License

GNU General Public License v3.0
