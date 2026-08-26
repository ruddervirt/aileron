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

## Documentation

- [End-user documentation](docs/end-user-documentation.md) — how to write `VirtualMachineBuild`, `VirtualMachineClone`, and `GradeRequest` manifests, plus the label/status conventions that tie them together. Start here to build/clone/grade VMs programmatically.
- [Platform documentation](docs/platform-documentation.md) — For operators deploying and administering an Aileron cluster.

## Developing

```bash
make build         # Build the Docker images locally
make push          # Build + push images to the registry
make helm-publish CHART_VERSION=1.2.3   # Package + push the versioned Helm chart
```

## License

GNU General Public License v3.0
