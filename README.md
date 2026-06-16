# Aileron

A Kubernetes operator for building VM disk images and cloning them on [KubeVirt](https://kubevirt.io/).

Aileron automates the full lifecycle: boot a VM from a cloud image or ISO, run provisioners (shell scripts, file uploads), capture the disk as a template, and clone it into running instances with isolated networking.

## Features

- **Multi-VM builds** — boot multiple VMs in parallel with isolated KubeOVN networking
- **Layered builds** — chain builds together, each layer adding to the previous
- **ISO installs** — install from ISO with EFI firmware and boot command automation
- **File provisioners** — upload files from ConfigMaps/Secrets into VMs
- **Shell provisioners** — run scripts over SSH with configurable commands
- **Snapshot cloning** — CSI volume snapshots for fast, space-efficient clones
- **Network isolation** — each build/clone gets its own KubeOVN VPC, subnets, and egress
- **S3 export** — optionally export built disk images to S3-compatible storage

## Prerequisites

- Kubernetes 1.30+ (tested on k3s 1.33)
- [KubeVirt](https://kubevirt.io/) 1.4+
- [CDI](https://github.com/kubevirt/containerized-data-importer) (Containerized Data Importer)
- [KubeOVN](https://kubeovn.github.io/docs/) for network isolation
- [Rook-Ceph](https://rook.io/) or another CSI driver with snapshot support (for cloning)

## Quick Start

```bash
# Deploy
helm install aileron ./chart/aileron \
  --namespace ruddervirt-system --create-namespace \
  --set image.tag=dev

# Create a build
kubectl apply -f - <<EOF
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: my-build
  namespace: ruddervirt-system
spec:
  vms:
    - name: server
      source:
        url: "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
      resources:
        cpu: 1
        memory: "1Gi"
        diskSize: "4Gi"
      communicator:
        sshUsername: debian
      cloudInit:
        userData: |
          #cloud-config
          ssh_authorized_keys: []
      provisioners:
        - name: setup
          type: shell
          shell:
            inline:
              - echo "Hello from aileron!"
              - sudo apt-get update && sudo apt-get install -y nginx
EOF

# Watch progress
kubectl get virtualmachinebuilds -n ruddervirt-system -w

# Clone the build
kubectl apply -f - <<EOF
apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineClone
metadata:
  name: my-clone
  namespace: ruddervirt-system
spec:
  templateName: my-build
EOF

# Watch the clone
kubectl get virtualmachineclones -n ruddervirt-system -w
```

## How It Works

### Building

1. The operator creates a KubeOVN VPC and subnet for the build
2. Source disk images are imported as DataVolumes
3. VMs boot with cloud-init (injecting SSH keys for provisioner access)
4. Shell and file provisioners run over SSH through a relay pod
5. VMs are shut down and their disks are cloned to output DataVolumes
6. Halted template VMs are created referencing the output disks

### Cloning

1. The operator validates the template build exists
2. CSI volume snapshots are taken of the template disks
3. New PVCs are created from the snapshots
4. A new KubeOVN VPC and subnet are created (derived from the template's topology)
5. Running VMs are created with the cloned disks and new network

### Resource Naming

Every build gets a unique ID like `vm-abc123` and every clone gets `ns-xyz789`. All resources are prefixed with this ID:

```
vm-abc123-server         # build VM (ephemeral) → reused for template VM
vm-abc123-out-server     # output disk
vm-abc123-default-nad    # network attachment
vm-abc123-vpc            # KubeOVN VPC

ns-xyz789-server         # cloned VM
ns-xyz789-out-server     # cloned disk
ns-xyz789-default-nad    # clone network attachment
ns-xyz789-vpc            # clone VPC
```

The short VM name (`server` above) is also recorded on every per-VM resource
as the `ruddervirt.io/vm` label, which is how the clone layer derives clone-side
names without having to parse the template VM's full name.

## Development

```bash
make build     # Build the Docker image
make push      # Build + push
make install   # Build + push + helm deploy
```

### Running Integration Tests

```bash
# Phase 1: independent builds (2 at a time)
kubectl apply -f test/integration/simple-build.yaml -f test/integration/file-provisioner.yaml
kubectl wait virtualmachinebuild/test-simple virtualmachinebuild/test-file-prov \
  -n ruddervirt-system --for=jsonpath='{.status.phase}'=Succeeded --timeout=60m

# Phase 2: layered builds (depend on phase 1)
kubectl apply -f test/integration/layered-simple.yaml -f test/integration/layered-build.yaml
kubectl wait virtualmachinebuild/test-layered-simple virtualmachinebuild/test-layered \
  -n ruddervirt-system --for=jsonpath='{.status.phase}'=Succeeded --timeout=60m

# Phase 3: clones
kubectl apply -f test/integration/clone-simple.yaml -f test/integration/clone-file-prov.yaml
kubectl wait virtualmachineclone/clone-simple virtualmachineclone/clone-file-prov \
  -n ruddervirt-system --for=jsonpath='{.status.phase}'=Ready --timeout=30m
```

## Configuration

### Helm Values

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/ruddervirt/aileron` | Operator image |
| `image.tag` | `v0.2.0` | Image tag |
| `vmResources.cpu` | `2` | CPU request per build/clone VM (controls scheduling concurrency) |
| `vmResources.memory` | `4096Mi` | Memory request per build/clone VM |
| `egressExternal.enabled` | `true` | Enable KubeOVN egress for internet access |
| `egressExternal.cidr` | `172.17.0.0/16` | Egress subnet CIDR |
| `egressExternal.gateway` | `172.17.0.1` | Egress gateway IP |
| `vncProxy.port` | `5900` | VNC proxy port for boot command automation |

## License

GNU General Public License v3.0
