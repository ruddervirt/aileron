// coordinator runs as a Job pod to handle the full post-boot lifecycle
// of a build VM: VNC boot commands followed by SSH provisioning.
// All blocking I/O (VNC, SSH) runs here, keeping the controller free.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr/funcr"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	"github.com/ruddervirt/aileron/internal/build/guacclient"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	interKeyDelay        = 100 * time.Millisecond
	keyPressReleaseDelay = 100 * time.Millisecond
	vncPollInterval      = 500 * time.Millisecond
	vncPollTimeout       = 10 * time.Minute

	sshRetryInterval   = 15 * time.Second
	rebootPollInterval = 5 * time.Second

	// rebootPerCallTimeout bounds each TrySSH/getBootTime/RunCommand so a
	// half-dead SSH session during shutdown can't hang the poll loop forever.
	// ExecConn.SetReadDeadline is a no-op, so without a per-call ctx deadline
	// a RunCommand can block indefinitely on a read that never returns.
	rebootPerCallTimeout = 15 * time.Second

	// rebootInitialSettleWait gives Windows time to actually begin shutting
	// down before we start probing. Probing too soon hits a dying SSH session
	// that returns the pre-reboot boot time and can block mid-command.
	rebootInitialSettleWait = 20 * time.Second

	// rebootMaxWait is the overall deadline for a normal reboot. Windows AD
	// DC promotion reliably needs 12–14 minutes for the post-promotion boot
	// (SYSVOL replication, AD DS finalization) before sshd is ready, so the
	// budget here has to clear that.
	rebootMaxWait = 15 * time.Minute

	// windowsUpdateRebootMaxWait is the overall deadline for a reboot triggered
	// by Windows Update. Cumulative updates (e.g. KB5044285) frequently sit at
	// "Working on updates, X% complete, don't turn off" for 20-45 minutes after
	// the first boot, during which sshd is not yet up, so we need a much larger
	// budget than a regular reboot. Sized at 2h to cover worst-case observed
	// stalls on slim Windows 11 builds.
	windowsUpdateRebootMaxWait = 120 * time.Minute

	// windowsUpdateMaxCycles caps the install→reboot loop to prevent
	// infinite cycling when Windows keeps requesting reboots without
	// converging.
	windowsUpdateMaxCycles = 25

	// Path on the Windows VM where the elevation wrapper is uploaded.
	elevatedScriptPath = "C:/Windows/Temp/aileron-elevated.ps1"

	// Provisioner step types referenced from the dispatch loop. Mirror the
	// canonical ProvisionerType constants in api/v1alpha1 — kept as local
	// strings so the comparison stays cheap and avoids cross-package noise.
	stepTypeReboot        = "reboot"
	stepTypeWindowsUpdate = "windows-update"
	stepTypeHandbuild     = "handbuild"
)

//nolint:gocyclo // entrypoint with boot command + provisioner dispatch
func main() {
	// Initialize controller-runtime logger so log.FromContext works in
	// library code (e.g. NewRelayDialFunc).
	ctrllog.SetLogger(funcr.New(func(prefix, args string) {
		fmt.Fprintf(os.Stderr, "%s\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), prefix, args)
	}, funcr.Options{}))

	buildName := requireEnv("BOOT_BUILD")
	configFile := envOr("COORDINATOR_CONFIG", "/etc/coordinator/config.json")

	data, err := os.ReadFile(configFile)
	if err != nil {
		log.Fatalf("reading config: %v", err)
	}
	var cfg build.CoordinatorConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parsing config: %v", err)
	}

	ctx := context.Background()

	// Phase 1: Boot commands (VNC keystrokes)
	if len(cfg.BootCommands) > 0 {
		runBootCommands(buildName, cfg.BootCommands)
	}

	// Phase 2: Provisioning (SSH)
	if len(cfg.Provisioners) > 0 {
		if err := runProvisioners(ctx, buildName, &cfg); err != nil {
			log.Fatalf("provisioning failed: %v", err)
		}
	}

	logInfo(buildName, "Coordinator completed successfully")
}

// =============================================================================
// Provisioning
// =============================================================================

//nolint:gocyclo // step dispatch loop with many provisioner types
func runProvisioners(ctx context.Context, buildName string, cfg *build.CoordinatorConfig) error {
	ns := envOr("BOOT_NS", "")
	relayPod := envOr("RELAY_POD_NAME", "")
	vmIP := envOr("VM_IP", "")
	vmiName := envOr("BOOT_VMI", "")
	nicName := envOr("VM_NIC_NAME", "default")
	statusCM := envOr("STATUS_CONFIGMAP", "")

	if relayPod == "" {
		return fmt.Errorf("RELAY_POD_NAME must be set for provisioning")
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}

	k8sClient, err := newK8sClient(restCfg)
	if err != nil {
		return fmt.Errorf("creating k8s client: %w", err)
	}

	// Resolve VM IP if not provided — poll VMI until an IP is assigned.
	if vmIP == "" && vmiName != "" {
		logInfo(buildName, "Waiting for VM IP", "vmi", vmiName, "nic", nicName)
		for {
			ip, ipErr := build.GetVMIPFromVMI(ctx, k8sClient, vmiName, ns, nicName)
			if ipErr == nil && ip != "" {
				vmIP = ip
				logInfo(buildName, "Resolved VM IP", "ip", vmIP)
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}

	port := cfg.Communicator.Port
	if port == 0 {
		port = 22
	}
	shell := v1alpha1.ShellType(cfg.Communicator.Shell)
	if shell == "" {
		shell = v1alpha1.ShellTypeBash
	}

	// Read SSH private key from mounted secret if available.
	var privateKey []byte
	if keyPath := envOr("SSH_KEY_PATH", "/etc/coordinator/ssh/id"); fileExists(keyPath) {
		privateKey, err = os.ReadFile(keyPath)
		if err != nil {
			return fmt.Errorf("reading SSH key: %w", err)
		}
	}

	dialFunc := build.NewRelayDialFunc(restCfg, ns, relayPod, vmIP, port)
	comm := &build.SSHCommunicator{
		Dial:       dialFunc,
		Username:   cfg.Communicator.Username,
		Password:   cfg.Communicator.Password,
		PrivateKey: privateKey,
		Port:       port,
		Shell:      shell,
	}

	// Resume from existing status if the pod restarted (Job retry).
	results := make([]build.CoordinatorStepResult, len(cfg.Provisioners))
	for i, p := range cfg.Provisioners {
		results[i] = build.CoordinatorStepResult{
			Index:  p.Index,
			Name:   p.Name,
			Type:   p.Type,
			Status: "Pending",
		}
	}
	if statusCM != "" {
		existing := readStatus(ctx, k8sClient, ns, statusCM)
		if existing != nil && len(existing.ProvisionerResults) == len(results) {
			for i, r := range existing.ProvisionerResults {
				if r.Status == build.PhaseSucceeded {
					results[i].Status = build.PhaseSucceeded
					results[i].Duration = r.Duration
					logInfo(buildName, "Resuming: skipping completed step", "index", strconv.Itoa(i), "step", r.Name)
				}
			}
		}
	}

	// Wait for SSH before running any provisioner steps.
	writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
		Phase:                "waiting-for-ssh",
		BootCommandsComplete: len(cfg.BootCommands) > 0,
		ProvisionerResults:   results,
	})
	logInfo(buildName, "Waiting for SSH communicator")
	setSSHMsg := func(msg string) {
		if statusCM == "" {
			return
		}
		cur := readStatus(ctx, k8sClient, ns, statusCM)
		if cur == nil || cur.SSHWaitMessage == msg {
			return
		}
		cur.SSHWaitMessage = msg
		writeStatus(ctx, k8sClient, ns, statusCM, *cur)
	}
	if err := waitForSSH(ctx, buildName, comm, setSSHMsg); err != nil {
		return fmt.Errorf("waiting for SSH: %w", err)
	}
	logInfo(buildName, "SSH communicator ready")

	// Upload elevation wrapper for Windows — OpenSSH gives admin users a
	// filtered (non-elevated) token, but many provisioner steps require
	// full elevation (Windows Update, registry, bcdedit, certificates).
	if comm.Shell == v1alpha1.ShellTypePowerShell {
		logInfo(buildName, "Uploading elevation wrapper")
		if err := comm.UploadFile(ctx, []byte(build.ElevatedRunnerScript), elevatedScriptPath); err != nil {
			return fmt.Errorf("uploading elevation wrapper: %w", err)
		}
	}

	writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
		Phase:                "provisioning",
		BootCommandsComplete: len(cfg.BootCommands) > 0,
		ProvisionerResults:   results,
	})

	for i, step := range cfg.Provisioners {
		if results[i].Status == build.PhaseSucceeded {
			continue
		}

		// Ensure SSH is up before each step. A previous step (or even a
		// non-reboot shell command) may have triggered a reboot.
		if step.Type != stepTypeReboot && step.Type != stepTypeWindowsUpdate && step.Type != stepTypeHandbuild {
			if err := waitForSSH(ctx, buildName, comm, setSSHMsg); err != nil {
				return fmt.Errorf("waiting for SSH before step %d: %w", i, err)
			}
		}

		results[i].Status = build.PhaseRunning
		writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
			Phase:                "provisioning",
			BootCommandsComplete: true,
			ProvisionerResults:   results,
		})

		logProvisionerEvent(buildName, "Running provisioner", i, step.Type, step.Name)
		start := time.Now()

		// Apply per-step timeout if configured, otherwise use parent context (unlimited).
		stepCtx := ctx
		var stepCancel context.CancelFunc
		if step.StepTimeout != "" {
			d, parseErr := time.ParseDuration(step.StepTimeout)
			if parseErr != nil {
				return fmt.Errorf("provisioner %d: invalid stepTimeout %q: %w", i, step.StepTimeout, parseErr)
			}
			stepCtx, stepCancel = context.WithTimeout(ctx, d)
		}

		var stepErr error
		switch step.Type {
		case "shell":
			stepErr = runShellStep(stepCtx, buildName, comm, step.Shell)
		case "file":
			stepErr = runFileStep(stepCtx, buildName, comm, step.File)
		case stepTypeReboot:
			stepErr = runRebootStep(stepCtx, buildName, comm, step.Reboot)
		case stepTypeWindowsUpdate:
			stepErr = runWindowsUpdateStep(stepCtx, buildName, comm, step.WindowsUpdate)
		case stepTypeHandbuild:
			stepErr = runHandbuildStep(stepCtx, buildName, k8sClient, ns, statusCM, step.Handbuild, i, results)
		default:
			stepErr = fmt.Errorf("unknown provisioner type: %s", step.Type)
		}

		if stepCancel != nil {
			stepCancel()
		}

		results[i].Duration = time.Since(start).Round(time.Millisecond).String()

		if stepErr != nil {
			results[i].Status = build.PhaseFailed
			results[i].Message = stepErr.Error()
			// Emit the failure as forwarded log messages BEFORE returning:
			// the error string in the status ConfigMap/CR never reaches the
			// UI provisioner log stream, so without these lines a failed
			// step shows "Running provisioner" followed by silence.
			logProvisionerEvent(buildName, "Provisioner step failed", i, step.Type, step.Name)
			logInfo(buildName, "Provisioner error", "error", stepErr.Error())
			writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
				Phase:                "failed",
				BootCommandsComplete: true,
				ProvisionerResults:   results,
				Error:                fmt.Sprintf("provisioner %d (%s) failed: %v", i, step.Name, stepErr),
			})
			return fmt.Errorf("provisioner %d (%s): %w", i, step.Name, stepErr)
		}

		results[i].Status = build.PhaseSucceeded
		results[i].Message = ""
		logProvisionerEvent(buildName, "Provisioner step completed", i, step.Type, step.Name)

		writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
			Phase:                "provisioning",
			BootCommandsComplete: true,
			ProvisionerResults:   results,
		})

		// After a reboot, re-resolve the VM's IP. KubeVirt updates the VMI's
		// status.interfaces from the guest agent after every boot, and some
		// post-reboot flows (notably AD DC promotion) assign a static IP that
		// differs from the pre-reboot DHCP lease. The relay dial closure
		// captured at startup pins the old IP, so without this refresh the
		// next SSH attempt silently dials a dead address.
		if (step.Type == stepTypeReboot || step.Type == stepTypeWindowsUpdate) && vmiName != "" {
			newIP, refreshErr := refreshVMIP(ctx, buildName, k8sClient, vmiName, ns, nicName, vmIP)
			if refreshErr != nil {
				logInfo(buildName, "Post-reboot IP refresh failed", "error", refreshErr.Error())
			} else if newIP != "" && newIP != vmIP {
				logInfo(buildName, "VM IP changed after reboot", "oldIP", vmIP, "newIP", newIP)
				vmIP = newIP
				comm.Dial = build.NewRelayDialFunc(restCfg, ns, relayPod, vmIP, port)
			}
		}
	}

	writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
		Phase:                "succeeded",
		BootCommandsComplete: true,
		ProvisionerResults:   results,
	})
	return nil
}

// bashOrShWrapper builds a portable shell snippet that:
//  1. Probes for bash at the standard locations and via PATH.
//  2. Logs to stderr exactly what it found (so failures are not a guessing
//     game — see /bin/bash status, /bin/sh symlink, and the resolved
//     interpreter in the build output before any user-script output).
//  3. Execs bash with the script if found, otherwise execs /bin/sh as a
//     last resort. The script's shebang only matters for kernel-level
//     re-execs (e.g. `exec sudo "$0"`) — initial invocation is dictated
//     by what we pick here.
//
// All `printf` go to fd 2 so stdout stays clean for any commands that need
// to capture it.
func bashOrShWrapper(remotePath string) string {
	// Single-quoted heredoc-free one-liner: simple enough that even dash
	// (Debian's default /bin/sh) can evaluate it as an SSH exec command.
	return fmt.Sprintf(`BASH=""; for p in /bin/bash /usr/bin/bash /usr/local/bin/bash; do `+
		`if [ -x "$p" ]; then BASH="$p"; break; fi; done; `+
		`if [ -z "$BASH" ]; then BASH="$(command -v bash 2>/dev/null || true)"; fi; `+
		`printf '[aileron] bash probe: /bin/bash=%%s /usr/bin/bash=%%s '`+
		`'/usr/local/bin/bash=%%s PATH-bash=%%s /bin/sh->%%s\n' `+
		`"$( [ -x /bin/bash ] && echo OK || echo MISSING )" `+
		`"$( [ -x /usr/bin/bash ] && echo OK || echo MISSING )" `+
		`"$( [ -x /usr/local/bin/bash ] && echo OK || echo MISSING )" `+
		`"$(command -v bash 2>/dev/null || echo MISSING)" `+
		`"$(readlink /bin/sh 2>/dev/null || echo none)" >&2; `+
		`if [ -n "$BASH" ]; then `+
		`printf '[aileron] interpreter: %%s\n' "$BASH" >&2; exec "$BASH" %s; `+
		`else `+
		`printf '[aileron] interpreter: /bin/sh (no bash found)\n' >&2; exec /bin/sh %s; `+
		`fi`, remotePath, remotePath)
}

func runShellStep(
	ctx context.Context, buildName string, comm *build.SSHCommunicator, step *build.CoordinatorShellStep,
) error {
	if step == nil {
		return fmt.Errorf("shell step config is nil")
	}

	if step.Inline == "" {
		logInfo(buildName, "Shell step has no commands")
		return nil
	}

	preview := strings.SplitN(step.Inline, "\n", 2)[0]
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	logInfo(buildName, "Running shell script", "preview", preview)

	// Upload as temp script and execute (avoids shell escaping issues).
	var remotePath, cleanup string
	var content string

	switch comm.Shell {
	case v1alpha1.ShellTypePowerShell:
		remotePath = "C:/Windows/Temp/aileron-provision.ps1"
		content = step.Inline
		cleanup = fmt.Sprintf(`powershell.exe -NoProfile -Command "Remove-Item -Force '%s'"`, remotePath)
	default:
		remotePath = "/tmp/aileron-provision.sh"
		// The shebang must remain on line 1: when a script does `exec sudo "$0"`
		// or any other kernel-level re-exec, the kernel reads the first line.
		// Prepending anything pushes the user's shebang down and forces a
		// /bin/sh fallback (dash on Debian) — which loses bashisms like $EUID.
		if strings.HasPrefix(step.Inline, "#!") {
			content = step.Inline
		} else {
			content = "#!/usr/bin/env bash\nset -e\n" + step.Inline
		}
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		cleanup = "rm -f " + remotePath
	}

	if err := comm.UploadFile(ctx, []byte(content), remotePath); err != nil {
		return fmt.Errorf("uploading script: %w", err)
	}

	if comm.Shell != v1alpha1.ShellTypePowerShell {
		if _, err := comm.RunCommand(ctx, "chmod +x "+remotePath, nil); err != nil {
			logInfo(buildName, "Warning: chmod +x failed")
		}
	}

	var fullCmd string
	usesElevation := false
	if strings.TrimSpace(step.ExecuteCommand) != "" {
		// An executeCommand override runs over the plain SSH session. On
		// Windows that session carries a UAC-filtered (non-elevated) token,
		// so HKLM writes, service control, bcdedit etc. will fail even for
		// admin accounts. Surface that loudly — it must never look like the
		// elevated path silently misbehaving.
		if comm.Shell == v1alpha1.ShellTypePowerShell {
			logInfo(buildName, "Elevation bypassed",
				"reason", "executeCommand is set; script runs over SSH with a UAC-filtered (non-elevated) token",
				"command", step.ExecuteCommand)
		}
		fullCmd = strings.ReplaceAll(step.ExecuteCommand, "{{ .Command }}", remotePath)
	} else {
		switch comm.Shell {
		case v1alpha1.ShellTypePowerShell:
			// The elevation wrapper drives its own command line through
			// build.RunElevated; nothing to construct here.
			usesElevation = true
		default:
			// Detect bash with diagnostics so any future "is bash actually
			// here?" question is answered in the build log itself. We probe
			// both fixed paths and PATH (Debian has /bin/bash, FreeBSD has
			// /usr/local/bin/bash, Alpine has /bin/bash if `bash` is
			// installed) and explicitly log which interpreter we selected.
			fullCmd = bashOrShWrapper(remotePath)
		}
	}

	if usesElevation {
		gotOutput := false
		exitCode, err := build.RunElevated(ctx, comm, elevatedScriptPath, remotePath, step.Env, func(line string) {
			gotOutput = true
			logInfo(buildName, "Shell output", "output", line)
		})
		if cleanup != "" {
			_, _ = comm.RunCommand(ctx, cleanup, nil)
		}
		if !gotOutput {
			logInfo(buildName, "Shell script produced no output")
		}
		if err != nil {
			return fmt.Errorf("shell script failed: %w", err)
		}
		if exitCode != 0 {
			return fmt.Errorf("shell script exited with code %d", exitCode)
		}
		return nil
	}

	gotOutput := false
	output, err := comm.RunCommandStreaming(ctx, fullCmd, step.Env, func(line string) {
		line = build.SanitizeLogLine(line)
		if build.IsBlankLogLine(line) {
			return
		}
		gotOutput = true
		logInfo(buildName, "Shell output", "output", line)
	})
	if cleanup != "" {
		_, _ = comm.RunCommand(ctx, cleanup, nil)
	}

	if !gotOutput {
		logInfo(buildName, "Shell script produced no output")
	}

	if err != nil {
		return fmt.Errorf("shell script failed: %w\noutput: %s", err, truncateStr(output, 500))
	}
	return nil
}

func runFileStep(
	ctx context.Context, buildName string, comm *build.SSHCommunicator, step *build.CoordinatorFileStep,
) error {
	if step == nil {
		return fmt.Errorf("file step config is nil")
	}

	content := step.Content
	if len(content) == 0 && step.URL != "" {
		logInfo(buildName, "Downloading file", "url", step.URL, "destination", step.Destination)
		var err error
		content, err = build.DownloadURL(ctx, step.URL)
		if err != nil {
			return fmt.Errorf("downloading %s: %w", step.URL, err)
		}
	}

	// Try direct SFTP first; on permission denied, fall back to uploading
	// to a temp path and using sudo (Linux) or admin move (Windows).
	if err := comm.UploadFile(ctx, content, step.Destination); err == nil {
		return nil
	}

	logInfo(buildName, "Direct SFTP failed, retrying with elevation", "destination", step.Destination)

	var tmpPath, cmd string
	switch comm.Shell {
	case v1alpha1.ShellTypePowerShell:
		tmpPath = fmt.Sprintf(`C:\Windows\Temp\aileron-upload-%d`, time.Now().UnixNano())
		cmd = fmt.Sprintf(
			`powershell.exe -NoProfile -Command "Move-Item -Force '%s' '%s'"`,
			tmpPath, step.Destination,
		)
	default:
		tmpPath = fmt.Sprintf("/tmp/aileron-upload-%d", time.Now().UnixNano())
		cmd = fmt.Sprintf("sudo install -m 0755 %s %s && rm -f %s", tmpPath, step.Destination, tmpPath)
	}
	if err := comm.UploadFile(ctx, content, tmpPath); err != nil {
		return fmt.Errorf("elevated upload to %s: %w", tmpPath, err)
	}
	if output, err := comm.RunCommand(ctx, cmd, nil); err != nil {
		return fmt.Errorf("elevated install to %s: %w\noutput: %s", step.Destination, err, output)
	}
	return nil
}

func runRebootStep(
	ctx context.Context, buildName string, comm *build.SSHCommunicator, step *build.CoordinatorRebootStep,
) error {
	// Wait for SSH so we can query boot time and send reboot.
	if err := waitForSSH(ctx, buildName, comm, nil); err != nil {
		return err
	}

	// Record pre-reboot boot time. Bound the call so a wedged SSH read can't
	// block the step before we even send the reboot.
	preCtx, preCancel := context.WithTimeout(ctx, rebootPerCallTimeout)
	preBootTime, err := getBootTime(preCtx, comm)
	preCancel()
	if err != nil {
		logInfo(buildName, "Failed to get boot time, will rely on SSH drop", "error", err.Error())
		preBootTime = ""
	}

	// Send reboot command. Ignore the error since the connection will drop —
	// but bound it so a refused shutdown can't hang here either.
	rebootCmd := "sudo reboot"
	if step != nil && step.Command != "" {
		rebootCmd = step.Command
	} else if comm.Shell == v1alpha1.ShellTypePowerShell {
		rebootCmd = "shutdown /r /f /t 0"
	}
	logInfo(buildName, "Sending reboot command", "command", rebootCmd, "bootTime", preBootTime)
	sendCtx, sendCancel := context.WithTimeout(ctx, rebootPerCallTimeout)
	_, _ = comm.RunCommand(sendCtx, rebootCmd, nil)
	sendCancel()

	return waitForReboot(ctx, buildName, comm, preBootTime, "Reboot", rebootMaxWait)
}

// waitForReboot blocks until SSH comes back AND the system boot time has
// flipped relative to preBootTime. It bounds every TrySSH and getBootTime
// call with a per-call timeout so a half-dead SSH session during shutdown
// or Windows update application cannot wedge the loop indefinitely, and
// enforces an overall deadline so a truly dead VM fails fast rather than
// blocking the whole build until its outer timeout expires.
//
// logPrefix tags the log messages so multiple reboot waits in the same
// build (e.g. a regular reboot step and a Windows Update reboot) stay
// distinguishable in the logs. maxWait lets the caller pick a budget
// appropriate to the scenario — regular reboots need ~10 minutes, Windows
// Update reboots can need 45-60 minutes for cumulative updates.
func waitForReboot(
	ctx context.Context, buildName string, comm *build.SSHCommunicator,
	preBootTime, logPrefix string, maxWait time.Duration,
) error {
	// Let the VM actually begin shutting down before probing. Probing
	// immediately catches a half-dead SSH session that still returns the
	// pre-reboot boot time and can block mid-command (ExecConn.SetReadDeadline
	// is a no-op in older builds, so a read with no ctx deadline can hang
	// indefinitely).
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(rebootInitialSettleWait):
	}

	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Bound each SSH probe. A stuck dial or dying handshake would
		// otherwise block the loop until the outer build timeout.
		tryCtx, tryCancel := context.WithTimeout(ctx, rebootPerCallTimeout)
		sshErr := comm.TrySSH(tryCtx)
		tryCancel()
		if sshErr != nil {
			logInfo(buildName, logPrefix+": SSH not ready", "error", sshErr.Error())
			if !sleepOrDone(ctx, rebootPollInterval) {
				return ctx.Err()
			}
			continue
		}

		// SSH is back — verify the boot time actually flipped.
		if preBootTime != "" {
			btCtx, btCancel := context.WithTimeout(ctx, rebootPerCallTimeout)
			currentBoot, err := getBootTime(btCtx, comm)
			btCancel()
			if err != nil {
				logInfo(buildName, logPrefix+": boot time query failed", "error", err.Error())
				if !sleepOrDone(ctx, rebootPollInterval) {
					return ctx.Err()
				}
				continue
			}
			if strings.TrimSpace(currentBoot) == strings.TrimSpace(preBootTime) {
				logInfo(buildName, logPrefix+": boot time unchanged, still waiting", "bootTime", currentBoot)
				if !sleepOrDone(ctx, rebootPollInterval) {
					return ctx.Err()
				}
				continue
			}
			logInfo(buildName, logPrefix+" confirmed", "preReboot", preBootTime, "current", currentBoot)
		} else {
			logInfo(buildName, logPrefix+": SSH back (no boot time to compare)")
		}
		return nil
	}

	logInfo(buildName, logPrefix+" did not complete within budget",
		"maxWait", maxWait.String(), "preBootTime", preBootTime)
	return fmt.Errorf("%s did not complete within %s (pre-reboot boot time: %q)", logPrefix, maxWait, preBootTime)
}

// sleepOrDone waits for d, returning false if ctx was canceled during the wait.
// Used by the reboot polling loop to stay responsive to cancellation. d is
// parameterized for clarity at call sites even though current callers all
// pass the same interval.
//
//nolint:unparam // see comment above
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// refreshVMIP polls the VMI's status.interfaces for a fresh IP after a
// reboot. KubeVirt's guest agent repopulates status.interfaces only once the
// guest is back up and the agent reattaches, so this may take 30–60s on
// Windows. Returns the observed IP (which may equal the prior IP) or an
// error if no IP is reported within the polling window.
func refreshVMIP(
	ctx context.Context, buildName string,
	k8sClient client.Client, vmiName, namespace, nicName, priorIP string,
) (string, error) {
	const (
		pollInterval = 5 * time.Second
		maxWait      = 90 * time.Second
	)
	logInfo(buildName, "Refreshing VM IP after reboot", "vmi", vmiName, "priorIP", priorIP)
	deadline := time.Now().Add(maxWait)
	var lastErr error
	for time.Now().Before(deadline) {
		ip, err := build.GetVMIPFromVMI(ctx, k8sClient, vmiName, namespace, nicName)
		if err == nil && ip != "" {
			return ip, nil
		}
		lastErr = err
		if !sleepOrDone(ctx, pollInterval) {
			return "", ctx.Err()
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("no IP reported within %s: %w", maxWait, lastErr)
	}
	return "", fmt.Errorf("no IP reported within %s", maxWait)
}

//nolint:gocyclo // Windows Update install/reboot loop
func runWindowsUpdateStep(
	ctx context.Context, buildName string, comm *build.SSHCommunicator,
	step *build.CoordinatorWindowsUpdateStep,
) error {
	if step == nil {
		step = &build.CoordinatorWindowsUpdateStep{}
	}

	searchCriteria := step.SearchCriteria
	if searchCriteria == "" {
		searchCriteria = "BrowseOnly=0 and IsInstalled=0"
	}
	updateLimit := step.UpdateLimit
	if updateLimit <= 0 {
		updateLimit = 1000
	}
	filters := step.Filters
	if len(filters) == 0 {
		filters = []string{"include:$true"}
	}

	// Upload the Windows Update script.
	scriptPath := "C:/Windows/Temp/aileron-windows-update.ps1"
	logInfo(buildName, "Uploading Windows Update script")
	if err := comm.UploadFile(ctx, []byte(build.WindowsUpdateScript), scriptPath); err != nil {
		return fmt.Errorf("uploading windows update script: %w", err)
	}

	// Build a wrapper script that calls the main script with hardcoded
	// parameters. This avoids all cmd.exe/PowerShell quoting issues when
	// passing arguments via SSH.
	quotedFilters := make([]string, len(filters))
	for i, f := range filters {
		quotedFilters[i] = fmt.Sprintf("    '%s'", strings.ReplaceAll(f, "'", "''"))
	}
	wrapper := fmt.Sprintf(
		"$filters = @(\n%s\n)\n& \"%s\" -SearchCriteria \"%s\" -Filters $filters -UpdateLimit %d\nexit $LASTEXITCODE\n",
		strings.Join(quotedFilters, ",\n"), scriptPath, searchCriteria, updateLimit,
	)

	wrapperPath := "C:/Windows/Temp/aileron-wu-run.ps1"
	if err := comm.UploadFile(ctx, []byte(wrapper), wrapperPath); err != nil {
		return fmt.Errorf("uploading wrapper script: %w", err)
	}

	cycle := 0
	for {
		cycle++
		if cycle > windowsUpdateMaxCycles {
			return fmt.Errorf("windows update did not converge after %d cycles", windowsUpdateMaxCycles)
		}
		logInfo(buildName, "Windows Update cycle", "cycle", strconv.Itoa(cycle), "criteria", searchCriteria)

		// Run the wrapper script elevated. Exit 101 = reboot needed, 0 = done.
		exitCode, err := build.RunElevated(ctx, comm, elevatedScriptPath, wrapperPath, nil, func(line string) {
			logInfo(buildName, "Windows Update output", "output", line)
		})
		if err != nil {
			return fmt.Errorf("windows update failed: %w", err)
		}

		switch exitCode {
		case 0:
			logInfo(buildName, "Windows Update complete, no more updates")
			cleanupCmd := fmt.Sprintf(
				`powershell.exe -NoProfile -Command "Remove-Item -Force '%s','%s'"`,
				scriptPath, wrapperPath,
			)
			_, _ = comm.RunCommand(ctx, cleanupCmd, nil)
			return nil
		case 101:
			logInfo(buildName, "Windows Update requires reboot", "cycle", strconv.Itoa(cycle))

			// Record the pre-reboot boot time with a bounded call so a
			// wedged session can't hang before we even send the reboot.
			preCtx, preCancel := context.WithTimeout(ctx, rebootPerCallTimeout)
			preBootTime, _ := getBootTime(preCtx, comm)
			preCancel()

			logInfo(buildName, "Sending reboot for Windows Update", "bootTime", preBootTime)
			sendCtx, sendCancel := context.WithTimeout(ctx, rebootPerCallTimeout)
			_, _ = comm.RunCommand(sendCtx, "shutdown /r /f /t 0", nil)
			sendCancel()

			// Cumulative updates can spend 20-45 minutes at "Working on
			// updates, X% complete, don't turn off" after the first boot
			// with sshd down the whole time — use the larger budget.
			logInfo(buildName, "Waiting for Windows Update reboot",
				"maxWait", windowsUpdateRebootMaxWait.String(), "cycle", strconv.Itoa(cycle))
			if err := waitForReboot(
				ctx, buildName, comm, preBootTime, "Windows Update reboot", windowsUpdateRebootMaxWait,
			); err != nil {
				return fmt.Errorf("waiting for windows update reboot: %w", err)
			}

			// Re-upload scripts (temp files may be cleared on reboot).
			if uploadErr := comm.UploadFile(ctx, []byte(build.ElevatedRunnerScript), elevatedScriptPath); uploadErr != nil {
				return fmt.Errorf("re-uploading elevation wrapper after reboot: %w", uploadErr)
			}
			if uploadErr := comm.UploadFile(ctx, []byte(build.WindowsUpdateScript), scriptPath); uploadErr != nil {
				return fmt.Errorf("re-uploading windows update script after reboot: %w", uploadErr)
			}
			if uploadErr := comm.UploadFile(ctx, []byte(wrapper), wrapperPath); uploadErr != nil {
				return fmt.Errorf("re-uploading wrapper script after reboot: %w", uploadErr)
			}

			continue // Next cycle
		default:
			return fmt.Errorf("windows update exited with unexpected code %d", exitCode)
		}
	}
}

const handbuildPollInterval = 5 * time.Second

func runHandbuildStep(
	ctx context.Context, buildName string, k8sClient client.Client,
	ns, statusCM string, step *build.CoordinatorHandbuildStep,
	stepIndex int, results []build.CoordinatorStepResult,
) error {
	instructions := ""
	if step != nil {
		instructions = step.Instructions
	}

	logInfo(buildName, "Handbuild step: waiting for continue signal", "instructions", instructions)

	// Write handbuild phase so the controller/UI knows we're paused.
	results[stepIndex].Message = instructions
	writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
		Phase:                "handbuild",
		BootCommandsComplete: true,
		ProvisionerResults:   results,
	})

	// Poll for continue signal.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(handbuildPollInterval):
		}

		status := readStatus(ctx, k8sClient, ns, statusCM)
		if status != nil && status.ContinueSignal {
			logInfo(buildName, "Handbuild step: continue signal received")
			// Clear the signal and restore provisioning phase.
			results[stepIndex].Message = ""
			writeStatus(ctx, k8sClient, ns, statusCM, build.CoordinatorStatus{
				Phase:                "provisioning",
				BootCommandsComplete: true,
				ProvisionerResults:   results,
			})
			return nil
		}
	}
}

func getBootTime(ctx context.Context, comm *build.SSHCommunicator) (string, error) {
	var cmd string
	switch comm.Shell {
	case v1alpha1.ShellTypePowerShell:
		cmd = `powershell.exe -NoProfile -Command "(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToString('o')"`
	default:
		cmd = "uptime -s"
	}
	out, err := comm.RunCommand(ctx, cmd, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// waitForSSH blocks until the VM accepts an SSH handshake.
// setWaitMessage, if non-nil, is invoked with a non-empty category string when
// the failure is actionable (authentication rejected) and with "" once SSH
// succeeds. Transient pre-boot errors (connection refused / timeout) are not
// reported through the callback to avoid spamming status during normal boot.
func waitForSSH(ctx context.Context, buildName string, comm *build.SSHCommunicator, setWaitMessage func(string)) error {
	for {
		if err := comm.TrySSH(ctx); err == nil {
			if setWaitMessage != nil {
				setWaitMessage("")
			}
			return nil
		} else {
			category := sshErrorCategory(err)
			logInfo(buildName, category, "error", err.Error())
			if setWaitMessage != nil && strings.Contains(err.Error(), "unable to authenticate") {
				setWaitMessage(category)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sshRetryInterval):
		}
	}
}

// sshErrorCategory returns a human-readable log message based on the SSH error.
func sshErrorCategory(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"):
		return "SSH authentication failed (check username/password)"
	case strings.Contains(msg, "connection refused"):
		return "SSH connection refused (port not open)"
	case strings.Contains(msg, "timed out"):
		return "SSH connection timed out (VM not reachable)"
	case strings.Contains(msg, "handshake failed: EOF"):
		return "SSH handshake failed (service starting up)"
	default:
		return "SSH not ready"
	}
}

// =============================================================================
// Status reporting
// =============================================================================

func readStatus(ctx context.Context, k8sClient client.Client, ns, cmName string) *build.CoordinatorStatus {
	cm := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ns}, cm); err != nil {
		return nil
	}
	raw, ok := cm.Data["status.json"]
	if !ok {
		return nil
	}
	var status build.CoordinatorStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		return nil
	}
	return &status
}

func writeStatus(ctx context.Context, k8sClient client.Client, ns, cmName string, status build.CoordinatorStatus) {
	data, err := json.Marshal(status)
	if err != nil {
		log.Printf("marshaling status: %v", err)
		return
	}

	cm := &corev1.ConfigMap{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ns}, cm)
	if errors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
			},
			Data: map[string]string{"status.json": string(data)},
		}
		if createErr := k8sClient.Create(ctx, cm); createErr != nil {
			log.Printf("creating status ConfigMap: %v", createErr)
		}
		return
	}
	if err != nil {
		log.Printf("getting status ConfigMap: %v", err)
		return
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["status.json"] = string(data)
	if updateErr := k8sClient.Update(ctx, cm); updateErr != nil {
		log.Printf("updating status ConfigMap: %v", updateErr)
	}
}

func newK8sClient(cfg *rest.Config) (client.Client, error) {
	return client.New(cfg, client.Options{Scheme: build.NewScheme()})
}

// =============================================================================
// Boot commands (VNC keystrokes) — unchanged from original bootcmd
// =============================================================================

func runBootCommands(buildName string, commands []string) {
	vncURL := requireEnv("VNC_URL")
	ns := requireEnv("BOOT_NS")
	vmi := requireEnv("BOOT_VMI")

	actions, err := build.ParseBootCommands(commands)
	if err != nil {
		log.Fatalf("parsing boot commands: %v", err)
	}

	wsURL := fmt.Sprintf("%s/internal/%s/%s", strings.TrimSuffix(vncURL, "/"), ns, vmi)
	logInfo(buildName, "Waiting for VNC", "vmi", vmi, "url", wsURL)

	gc, err := connectGuac(wsURL)
	if err != nil {
		log.Fatalf("connecting to VNC gateway: %v", err)
	}
	defer func() { _ = gc.Close() }()

	logInfo(buildName, "Sending boot commands", "vmi", vmi, "actions", strconv.Itoa(len(actions)))

	// reconnect re-establishes the gateway connection after a transient failure.
	reconnect := func(cause error) {
		_ = gc.Close()
		logInfo(buildName, "VNC connection lost, reconnecting", "url", wsURL, "error", cause.Error())
		newGC, err := connectGuac(wsURL)
		if err != nil {
			log.Fatalf("reconnecting to VNC gateway: %v", err)
		}
		gc = newGC
		logInfo(buildName, "VNC reconnected")
	}

	// sendKey sends a key event, reconnecting once on failure.
	sendKey := func(keysym uint32, down bool) {
		if err := gc.SendKey(keysym, down); err != nil {
			reconnect(err)
			if err := gc.SendKey(keysym, down); err != nil {
				log.Fatalf("sending key event after reconnect: %v", err)
			}
		}
	}

	var typedBuf strings.Builder
	flushTyped := func() {
		if typedBuf.Len() > 0 {
			logInfo(buildName, "Boot command", "action", "type", "keys", typedBuf.String())
			typedBuf.Reset()
		}
	}

	for _, action := range actions {
		switch a := action.(type) {
		case build.WaitAction:
			flushTyped()
			logInfo(buildName, "Boot command", "action", "wait", "duration", a.Duration.String())
			time.Sleep(a.Duration)

		case build.KeyAction:
			if name, ok := build.KeysymNames[a.Keysym]; ok {
				flushTyped()
				if a.Down {
					logInfo(buildName, "Boot command", "action", "key_down", "key", name)
				} else if a.Up {
					logInfo(buildName, "Boot command", "action", "key_up", "key", name)
				} else {
					logInfo(buildName, "Boot command", "action", "key", "key", name)
				}
			} else if a.Keysym >= 0x20 && a.Keysym <= 0x7e && !a.Down && !a.Up {
				typedBuf.WriteRune(rune(a.Keysym))
			}

			if a.Down {
				sendKey(a.Keysym, true)
			} else if a.Up {
				sendKey(a.Keysym, false)
			} else {
				sendKey(a.Keysym, true)
				time.Sleep(keyPressReleaseDelay)
				sendKey(a.Keysym, false)
			}
			time.Sleep(interKeyDelay)
		}
	}
	flushTyped()

	logInfo(buildName, "Boot commands sent successfully", "vmi", vmi)
}

// =============================================================================
// VNC helpers
// =============================================================================

// connectGuac dials the vncgateway's internal Guacamole endpoint, polling
// until the VNC session is live (guacd connected to the VMI) or the timeout
// expires. Mirrors the old raw-RFB readiness semantics: a WebSocket that
// closes before the first server instruction counts as "not up yet".
func connectGuac(url string) (*guacclient.Client, error) {
	deadline := time.Now().Add(vncPollTimeout)
	for {
		// Generous per-attempt budget: the gateway holds the connection with
		// keepalive nops until the console exists, so most of an attempt is
		// quiet server-side waiting, not failure.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		gc, err := guacclient.Dial(ctx, url)
		cancel()
		if err == nil {
			return gc, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("VNC not available after %s: last error: %v", vncPollTimeout, err)
		}
		time.Sleep(vncPollInterval)
	}
}

// =============================================================================
// Utilities
// =============================================================================

// vmName is the short VM spec name (e.g. "server", "client") read from
// BOOT_VM_NAME at startup. Included in every log line so multi-VM builds
// can be distinguished in aggregated logs.
var vmName string

func init() {
	vmName = os.Getenv("BOOT_VM_NAME")
}

func logInfo(buildName, msg string, kvs ...string) {
	fields := map[string]string{"name": buildName}
	if vmName != "" {
		fields["vm"] = vmName
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		fields[kvs[i]] = kvs[i+1]
	}
	j, _ := json.Marshal(fields)
	_, _ = fmt.Fprintf(os.Stdout, "%s\tINFO\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), msg, string(j))
}

// logProvisionerEvent outputs a provisioner log line with proper JSON types
// matching the format an external log-streamer parser expects:
// {"name": "<buildName>", "vm": "<vmName>", "index": <int>, "type": "<string>", "step": "<stepName>"}
func logProvisionerEvent(buildName, msg string, index int, stepType, stepName string) {
	fields := map[string]any{
		"name":  buildName,
		"index": index,
		"type":  stepType,
		"step":  stepName,
	}
	if vmName != "" {
		fields["vm"] = vmName
	}
	j, _ := json.Marshal(fields)
	_, _ = fmt.Fprintf(os.Stdout, "%s\tINFO\t%s\t%s\n", time.Now().UTC().Format(time.RFC3339), msg, string(j))
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
