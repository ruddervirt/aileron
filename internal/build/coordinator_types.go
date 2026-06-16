package build

// CoordinatorConfig is the JSON config written to a ConfigMap and read by the
// coordinator pod. The controller pre-resolves all references (scriptFrom,
// fileRef, URLs) so the coordinator has no Kubernetes API dependencies beyond
// pods/exec (for the relay tunnel) and configmaps (for status writes).
type CoordinatorConfig struct {
	// BootCommands are Packer-style VNC keystroke strings (e.g. "<enter>", "<wait5>").
	// Empty means skip the boot command phase.
	BootCommands []string `json:"bootCommands,omitempty"`

	// Provisioners is the ordered list of provisioning steps.
	// Empty means skip the provisioning phase.
	Provisioners []CoordinatorProvisioner `json:"provisioners,omitempty"`

	// Communicator configures SSH access to the VM.
	Communicator CoordinatorCommunicator `json:"communicator"`
}

// CoordinatorProvisioner is a single provisioning step with all content
// pre-resolved (inline scripts, file content as base64, etc.).
type CoordinatorProvisioner struct {
	Index       int    `json:"index"`
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	StepTimeout string `json:"stepTimeout,omitempty"`

	// Shell step (type=shell).
	Shell *CoordinatorShellStep `json:"shell,omitempty"`
	// File step (type=file).
	File *CoordinatorFileStep `json:"file,omitempty"`
	// Reboot step (type=reboot).
	Reboot *CoordinatorRebootStep `json:"reboot,omitempty"`
	// WindowsUpdate step (type=windows-update).
	WindowsUpdate *CoordinatorWindowsUpdateStep `json:"windowsUpdate,omitempty"`
	// Handbuild step (type=handbuild).
	Handbuild *CoordinatorHandbuildStep `json:"handbuild,omitempty"`
}

// CoordinatorShellStep contains a pre-resolved shell script.
type CoordinatorShellStep struct {
	Inline         string            `json:"inline,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	ExecuteCommand string            `json:"executeCommand,omitempty"`
}

// CoordinatorFileStep contains file content or a URL to download at runtime.
// Inline content is embedded directly. URL-sourced files are downloaded by
// the coordinator pod to avoid hitting the ConfigMap size limit.
type CoordinatorFileStep struct {
	// Content is the raw file bytes (inline files only).
	Content []byte `json:"content,omitempty"`
	// URL is downloaded by the coordinator at runtime (large files).
	URL         string `json:"url,omitempty"`
	Destination string `json:"destination"`
}

// CoordinatorRebootStep configures a reboot.
type CoordinatorRebootStep struct {
	Command string `json:"command,omitempty"`
}

// CoordinatorWindowsUpdateStep configures a Windows Update cycle.
type CoordinatorWindowsUpdateStep struct {
	SearchCriteria string   `json:"searchCriteria,omitempty"`
	Filters        []string `json:"filters,omitempty"`
	UpdateLimit    int      `json:"updateLimit,omitempty"`
}

// CoordinatorHandbuildStep pauses the build for human VNC interaction.
type CoordinatorHandbuildStep struct {
	Instructions string `json:"instructions"`
}

// CoordinatorCommunicator configures SSH access for the coordinator pod.
type CoordinatorCommunicator struct {
	Shell    string `json:"shell"`
	Username string `json:"username"`
	Port     int32  `json:"port"`
	Password string `json:"password,omitempty"`
	// PrivateKey is read from a mounted Secret volume, not from config.
}

// CoordinatorStatus is the JSON status written by the coordinator pod to a
// ConfigMap. The controller reads this to update ProvisionerResults.
type CoordinatorStatus struct {
	Phase                string                  `json:"phase"` // "bootcmd", "provisioning", "handbuild", "succeeded", "failed"
	ContinueSignal       bool                    `json:"continueSignal,omitempty"`
	BootCommandsComplete bool                    `json:"bootCommandsComplete"`
	ProvisionerResults   []CoordinatorStepResult `json:"provisionerResults,omitempty"`
	Error                string                  `json:"error,omitempty"`
	// SSHWaitMessage surfaces actionable SSH failures (e.g. authentication
	// rejection) while the coordinator is waiting for the communicator.
	// Left empty during normal pre-boot retries (connection refused, timeout)
	// since those are expected during VM startup.
	SSHWaitMessage string `json:"sshWaitMessage,omitempty"`
}

// CoordinatorStepResult is the status of a single provisioner step.
type CoordinatorStepResult struct {
	Index    int    `json:"index"`
	Name     string `json:"name,omitempty"`
	Type     string `json:"type,omitempty"`
	Status   string `json:"status"` // "Pending", "Running", "Succeeded", "Failed"
	Message  string `json:"message,omitempty"`
	Duration string `json:"duration,omitempty"`
}
