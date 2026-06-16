package build

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// CoordinatorHandler orchestrates the full post-boot VM lifecycle by
// launching a coordinator Job pod that handles VNC boot commands and SSH
// provisioning. The controller never touches SSH — it only polls status.
type CoordinatorHandler struct {
	Client     client.Client
	RESTConfig *rest.Config
}

// HandleVM is called for both VMPhaseBootCommand and VMPhaseProvisioning.
// It creates the coordinator Job (once) and polls its status + results ConfigMap.
func (h *CoordinatorHandler) HandleVM(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (v1alpha1.VMPhase, error) {
	logger := log.FromContext(ctx)
	buildID := BuildID(vmBuild)
	buildNS := BuildNS(vmBuild)

	vmiName := vmStatus.VMName
	if vmiName == "" {
		vmiName = BuildNameForBuildVM(buildID, vmSpec.Name)
	}

	// Ensure the coordinator Job exists.
	if err := h.ensureCoordinatorResources(ctx, vmBuild, vmSpec, vmStatus); err != nil {
		return v1alpha1.VMPhaseFailed, err
	}

	// Check Job completion.
	status, msg, err := CoordinatorJobStatus(ctx, h.Client, h.RESTConfig, vmBuild, vmSpec.Name)
	if err != nil {
		return v1alpha1.VMPhaseFailed, fmt.Errorf("checking coordinator Job: %w", err)
	}

	// Read status ConfigMap for step-level progress.
	coordStatus := h.readStatus(ctx, buildNS, CoordinatorStatusConfigMapName(buildID, vmSpec.Name))
	if coordStatus != nil {
		// Sync provisioner results back to the CRD status.
		syncProvisionerResults(vmStatus, coordStatus)
	}

	switch status {
	case PhaseSucceeded:
		logger.Info("Coordinator Job completed", "vmi", vmiName)
		return v1alpha1.VMPhaseShuttingDown, nil
	case PhaseFailed:
		return v1alpha1.VMPhaseFailed, fmt.Errorf("coordinator Job failed: %s", msg)
	default:
		// Determine sub-phase from coordinator status.
		if coordStatus != nil {
			switch coordStatus.Phase {
			case "provisioning":
				vmStatus.Message = "Provisioning"
				return v1alpha1.VMPhaseProvisioning, nil
			case "waiting-for-ssh":
				if coordStatus.SSHWaitMessage != "" {
					vmStatus.Message = coordStatus.SSHWaitMessage
				} else {
					vmStatus.Message = "Waiting for communicator"
				}
				return v1alpha1.VMPhaseProvisioning, nil
			case "handbuild":
				vmStatus.Message = "Waiting for handbuild (VNC)"
				return v1alpha1.VMPhaseProvisioning, nil
			}
		}
		vmStatus.Message = "Running boot commands"
		return v1alpha1.VMPhaseBootCommand, nil
	}
}

// EnsureJob pre-creates the coordinator ConfigMap and Job so the image is
// pulled while the VM is still booting. Called from VMPhaseBooting.
func (h *CoordinatorHandler) EnsureJob(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) error {
	if len(vmSpec.BootCommand) == 0 && len(vmSpec.Provisioners) == 0 {
		return nil
	}
	return h.ensureCoordinatorResources(ctx, vmBuild, vmSpec, vmStatus)
}

func (h *CoordinatorHandler) ensureCoordinatorResources(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) error {
	buildID := BuildID(vmBuild)

	vmiName := vmStatus.VMName
	if vmiName == "" {
		vmiName = BuildNameForBuildVM(buildID, vmSpec.Name)
	}

	// Build the coordinator config with pre-resolved content.
	cfg, err := h.buildConfig(ctx, vmBuild, vmSpec)
	if err != nil {
		return fmt.Errorf("building coordinator config: %w", err)
	}

	// Create ConfigMap.
	if err := EnsureCoordinatorConfigMap(ctx, h.Client, vmBuild, vmSpec, cfg); err != nil {
		return fmt.Errorf("creating coordinator ConfigMap: %w", err)
	}

	relayPodName := RelayPodName(buildID)
	statusCMName := CoordinatorStatusConfigMapName(buildID, vmSpec.Name)

	sshPort := vmSpec.Communicator.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}

	// NIC name for the coordinator to resolve VM IP from the VMI. The
	// relay reaches VMs only over OVN-IPAM'd ports, and unmanaged subnets
	// exclude their CIDR from IPAM - so the SSH NIC must be a managed one.
	nicName := "default"
	nics := effectiveVMNICs(vmBuild, vmSpec)
	if len(nics) > 0 {
		if mgmt, ok := FirstManagedNIC(vmBuild, nics); ok {
			nicName = mgmt.Name
		} else {
			return fmt.Errorf("VM %q has no managed NIC; the relay cannot reach an unmanaged-only VM", vmSpec.Name)
		}
	}

	// Create the coordinator Job — it resolves VM_IP itself via the k8s API.
	jobOpts := CoordinatorJobOpts{
		VMIName:      vmiName,
		VNCURL:       vncServiceURL(),
		RelayPodName: relayPodName,
		NICName:      nicName,
		SSHPort:      sshPort,
		StatusCMName: statusCMName,
		SSHKeySecret: SSHKeySecretName(buildID),
	}
	if err := EnsureCoordinatorJob(ctx, h.Client, vmBuild, vmSpec, jobOpts); err != nil {
		return fmt.Errorf("creating coordinator Job: %w", err)
	}

	return nil
}

// buildConfig pre-resolves all provisioner content into a CoordinatorConfig.
func (h *CoordinatorHandler) buildConfig(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (*CoordinatorConfig, error) {
	cfg := &CoordinatorConfig{}

	// Boot commands — always resolve {{ .HTTPIP }} and {{ .HTTPPort }}
	// templates when they appear. The relay pod's HTTP server is available
	// for every build with NICs, regardless of whether httpDirectory is
	// explicitly configured.
	if len(vmSpec.BootCommand) > 0 {
		commands := vmSpec.BootCommand
		if len(effectiveVMNICs(vmBuild, vmSpec)) > 0 {
			httpIP, err := GetRelayHTTPIP(ctx, h.Client, vmBuild, vmSpec)
			if err != nil {
				return nil, fmt.Errorf("resolving relay HTTP IP for boot command templates: %w", err)
			}
			commands = ResolveBootCommandTemplates(commands, map[string]string{
				"HTTPIP":   httpIP,
				"HTTPPort": fmt.Sprintf("%d", HTTPPort),
			})
		}
		cfg.BootCommands = commands
	}

	// Communicator.
	shell := string(vmSpec.Communicator.Shell)
	if shell == "" {
		shell = "bash"
	}
	username := vmSpec.Communicator.SSHUsername
	if username == "" {
		username = "root"
	}
	cfg.Communicator = CoordinatorCommunicator{
		Shell:    shell,
		Username: username,
		Port:     vmSpec.Communicator.SSHPort,
		Password: vmSpec.Communicator.SSHPassword,
	}

	// Provisioners — pre-resolve all content.
	for i, step := range vmSpec.Provisioners {
		p := CoordinatorProvisioner{
			Index:       i,
			Type:        string(step.Type),
			Name:        step.Name,
			StepTimeout: step.StepTimeout,
		}

		switch step.Type {
		case v1alpha1.ProvisionerTypeShell:
			if step.Shell != nil {
				inline := step.Shell.Inline
				if step.Shell.ScriptFrom != nil {
					loader := &K8sScriptLoader{Client: h.Client, Namespace: vmBuild.Namespace}
					script, err := loader.LoadScript(ctx, step.Shell.ScriptFrom, "")
					if err != nil {
						return nil, fmt.Errorf("provisioner %d: loading script: %w", i, err)
					}
					inline = script
				}
				shellStep := &CoordinatorShellStep{
					Inline:         inline,
					Env:            step.Shell.Env,
					ExecuteCommand: step.Shell.ExecuteCommand,
				}
				p.Shell = shellStep
			}

		case v1alpha1.ProvisionerTypeFile:
			if step.File != nil {
				fileStep, err := h.resolveFileStep(ctx, vmBuild, step.File)
				if err != nil {
					return nil, fmt.Errorf("provisioner %d: resolving file: %w", i, err)
				}
				p.File = fileStep
			}

		case v1alpha1.ProvisionerTypeReboot:
			reboot := &CoordinatorRebootStep{}
			if step.Reboot != nil {
				reboot.Command = step.Reboot.Command
			}
			p.Reboot = reboot

		case v1alpha1.ProvisionerTypeWindowsUpdate:
			wu := &CoordinatorWindowsUpdateStep{}
			if step.WindowsUpdate != nil {
				wu.SearchCriteria = step.WindowsUpdate.SearchCriteria
				wu.Filters = step.WindowsUpdate.Filters
				wu.UpdateLimit = step.WindowsUpdate.UpdateLimit
			}
			p.WindowsUpdate = wu

		case v1alpha1.ProvisionerTypeHandbuild:
			hb := &CoordinatorHandbuildStep{}
			if step.Handbuild != nil {
				hb.Instructions = step.Handbuild.Instructions
			}
			p.Handbuild = hb
		}

		cfg.Provisioners = append(cfg.Provisioners, p)
	}

	return cfg, nil
}

func (h *CoordinatorHandler) resolveFileStep(ctx context.Context, vmBuild *v1alpha1.VirtualMachineBuild, step *v1alpha1.FileProvisioner) (*CoordinatorFileStep, error) {
	if step.FileRef != "" {
		bf, err := ResolveFile(vmBuild, step.FileRef)
		if err != nil {
			return nil, err
		}
		if bf.Inline != "" {
			return &CoordinatorFileStep{Content: []byte(bf.Inline), Destination: step.Destination}, nil
		}
		if bf.URL != "" {
			// Pass the URL through — the coordinator downloads at runtime
			// to avoid hitting the ConfigMap size limit.
			return &CoordinatorFileStep{URL: bf.URL, Destination: step.Destination}, nil
		}
		return nil, fmt.Errorf("file %q has neither inline nor url content", step.FileRef)
	}
	if step.Source != nil {
		loader := &K8sScriptLoader{Client: h.Client, Namespace: vmBuild.Namespace}
		content, err := loader.LoadScript(ctx, step.Source, "")
		if err != nil {
			return nil, err
		}
		return &CoordinatorFileStep{Content: []byte(content), Destination: step.Destination}, nil
	}
	return nil, fmt.Errorf("file provisioner has neither fileRef nor source")
}

// readStatus reads the coordinator status ConfigMap. Returns nil if not found.
func (h *CoordinatorHandler) readStatus(ctx context.Context, ns, cmName string) *CoordinatorStatus {
	cm := &corev1.ConfigMap{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ns}, cm); err != nil {
		return nil
	}
	raw, ok := cm.Data["status.json"]
	if !ok {
		return nil
	}
	var status CoordinatorStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return nil
	}
	return &status
}

// syncProvisionerResults copies coordinator step results into the CRD status.
func syncProvisionerResults(vmStatus *v1alpha1.VMBuildStatus, coordStatus *CoordinatorStatus) {
	if len(coordStatus.ProvisionerResults) == 0 {
		return
	}
	// Ensure the results slice is the right size.
	for len(vmStatus.ProvisionerResults) < len(coordStatus.ProvisionerResults) {
		vmStatus.ProvisionerResults = append(vmStatus.ProvisionerResults, v1alpha1.ProvisionerResult{})
	}
	for _, r := range coordStatus.ProvisionerResults {
		if r.Index >= 0 && r.Index < len(vmStatus.ProvisionerResults) {
			pr := &vmStatus.ProvisionerResults[r.Index]
			pr.Index = int32(r.Index)
			pr.Status = r.Status
			pr.Message = r.Message
			pr.Name = r.Name
			if r.Type != "" {
				pr.Type = v1alpha1.ProvisionerType(r.Type)
			}
			// Duration is already set by the controller init; coordinator duration
			// is informational and included in the status ConfigMap.
		}
	}
}

// CoordinatorStatusConfigMapName returns the name of the status ConfigMap.
func CoordinatorStatusConfigMapName(buildID, vmName string) string {
	return fmt.Sprintf("%s-coordinator-status-%s", buildID, vmName)
}

// vncServiceURL returns the in-cluster URL of the vncgateway's internal
// (unauthenticated) listener. The chart normally injects VNC_SERVICE_URL on
// the manager; the default only covers the standard release name.
func vncServiceURL() string {
	if url := os.Getenv("VNC_SERVICE_URL"); url != "" {
		return url
	}
	ns := os.Getenv("OPERATOR_NAMESPACE")
	if ns == "" {
		ns = "ruddervirt-system"
	}
	return fmt.Sprintf("ws://aileron-vncgateway.%s.svc:%d", ns, 7778)
}
