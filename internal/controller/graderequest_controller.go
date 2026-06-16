/*
Copyright 2026.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

const (
	gradeDefaultResyncPeriod = 3 * time.Second
	gradeRequeueBackoff      = 1 * time.Second
	gradeJobActiveDeadline   = int64(300) // 5 minutes
	gradeJobBackoffLimit     = int32(0)   // no retries
	gradeCompletedRetention  = 5 * time.Minute
	gradeBootWaitDefault     = 90 * time.Second // guest-boot grace after auto power-on
	gradeBootTimeout         = 5 * time.Minute  // auto-started VM must reach Running within this
	gradeMaxConcurrentReconc = 10
	gradeVMIPhaseRunning     = "Running"
	gradeServiceAccountName  = "grader"
	gradeDefaultGraderImage  = "ghcr.io/ruddervirt/aileron/grader:latest"
)

// errGradeRequeue signals that a grade request needs a fast re-reconcile
// (a VM is still booting, or another grade job is holding a target VM).
var errGradeRequeue = errors.New("requeue requested")

// gradeKubevirtVMGVR is the GroupVersionResource used to look up
// VirtualMachines for OS-label resolution.
var gradeKubevirtVMGVR = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "virtualmachines",
}

// gradeKubevirtVMIGVR is used to read VirtualMachineInstance status.phase for
// power-state detection.
var gradeKubevirtVMIGVR = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "virtualmachineinstances",
}

func gradeGraderImage() string {
	if img := os.Getenv("GRADER_IMAGE"); img != "" {
		return img
	}
	return gradeDefaultGraderImage
}

// gradeBootWaitDuration is the grace period between auto powering a VM on and
// grading it, giving the guest OS time to bring up its serial console.
// Overridable via GRADER_BOOT_WAIT_SECONDS.
func gradeBootWaitDuration() time.Duration {
	if v := os.Getenv("GRADER_BOOT_WAIT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return gradeBootWaitDefault
}

// GradeRequestReconciler reconciles a GradeRequest object. It schedules a
// per-VM grading Job, drives the VM power state, and records the results.
type GradeRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RESTConfig is retained for the KubeVirt subresource REST client:
	// start/stop are custom action subresources the typed client cannot reach.
	RESTConfig *rest.Config
	// DynamicClient reads VirtualMachine/VirtualMachineInstance objects.
	DynamicClient dynamic.Interface
	// Clientset drives Jobs and streams grader pod logs (logs are not exposed
	// through the controller-runtime client).
	Clientset kubernetes.Interface
}

// +kubebuilder:rbac:groups=ruddervirt.io,resources=graderequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ruddervirt.io,resources=graderequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ruddervirt.io,resources=graderequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/start;virtualmachines/stop,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get;list

func (r *GradeRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gr := &v1alpha1.GradeRequest{}
	if err := r.Get(ctx, req.NamespacedName, gr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.reconcile(ctx, gr); err != nil {
		if errors.Is(err, errGradeRequeue) {
			return ctrl.Result{RequeueAfter: gradeRequeueBackoff}, nil
		}
		return ctrl.Result{}, err
	}

	// Boot waits and the post-completion retention timer are driven by
	// RequeueAfter, since neither VMI phase changes nor the retention clock
	// generate watch events.
	switch gr.Status.Phase {
	case v1alpha1.GradeRequestPhasePending, v1alpha1.GradeRequestPhaseRunning:
		return ctrl.Result{RequeueAfter: gradeDefaultResyncPeriod}, nil
	case v1alpha1.GradeRequestPhaseReady:
		if gr.Status.CompletedAt != nil {
			return ctrl.Result{RequeueAfter: gradeCompletedRetention}, nil
		}
	}
	return ctrl.Result{}, nil
}

// reconcile runs the phase state machine. Pending falls through to Running
// within a single pass once all grading jobs are scheduled.
func (r *GradeRequestReconciler) reconcile(ctx context.Context, gr *v1alpha1.GradeRequest) error {
	for {
		switch gr.Status.Phase {
		case "", v1alpha1.GradeRequestPhasePending:
			if err := r.handlePendingPhase(ctx, gr); err != nil {
				return err
			}
		case v1alpha1.GradeRequestPhaseRunning:
			return r.handleRunningPhase(ctx, gr)
		case v1alpha1.GradeRequestPhaseReady:
			return r.cleanupIfExpired(ctx, gr)
		case v1alpha1.GradeRequestPhaseFailed:
			return nil
		default:
			return fmt.Errorf("unknown phase %q for grade request %s/%s", gr.Status.Phase, gr.Namespace, gr.Name)
		}
	}
}

// normalizeSpec converts the legacy single-VM spec fields into a one-element
// VMs slice so the rest of the controller only needs to handle the multi-VM path.
func normalizeGradeSpec(spec *v1alpha1.GradeRequestSpec) {
	if len(spec.VMs) > 0 {
		return
	}
	if spec.VMName == "" {
		return
	}
	spec.VMs = []v1alpha1.GradeVM{{
		Name:     spec.VMName,
		Commands: spec.Commands,
		User:     spec.User,
		Password: spec.Password,
		Domain:   spec.Domain,
	}}
}

// filterGradableVMs drops VM entries that carry no commands. Callers such as
// rudder-ui enumerate every VM in the namespace but only some have grading
// checks defined; an empty command list means "nothing to grade here", not a
// malformed request. A VM that has commands but no name is still rejected. The
// returned reason is non-empty when the whole request should be failed.
func filterGradableVMs(vms []v1alpha1.GradeVM) (gradable []v1alpha1.GradeVM, reason string) {
	filtered := vms[:0]
	for _, vm := range vms {
		if len(vm.Commands) == 0 {
			continue
		}
		if vm.Name == "" {
			return nil, "each VM entry with commands must have a name"
		}
		filtered = append(filtered, vm)
	}
	if len(filtered) == 0 {
		return nil, "spec.vms must contain at least one VM with commands"
	}
	return filtered, ""
}

// resolveGradeMethod reads the ruddervirt.io/os label off the target VM and
// returns the corresponding GRADE_METHOD value. Aileron stamps this at build
// time so the grade request no longer needs the caller to specify it.
func (r *GradeRequestReconciler) resolveGradeMethod(ctx context.Context, namespace, vmName string) (string, error) {
	vm, err := r.DynamicClient.Resource(gradeKubevirtVMGVR).Namespace(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get VM %s/%s: %w", namespace, vmName, err)
	}
	switch vm.GetLabels()["ruddervirt.io/os"] {
	case "windows":
		return "SERIAL_WINDOWS", nil
	case "linux":
		return "SERIAL_LINUX", nil
	case "":
		return "", fmt.Errorf("VM %s/%s has no ruddervirt.io/os label — rebuild the VM or set the label manually", namespace, vmName)
	default:
		return "", fmt.Errorf("VM %s/%s has unrecognized ruddervirt.io/os label %q", namespace, vmName, vm.GetLabels()["ruddervirt.io/os"])
	}
}

// gradeVMIsOff reports whether a VMI phase represents a powered-off VM. A
// missing VMI ("") means the VM is stopped; Succeeded/Failed VMIs linger after
// a guest shutdown until the VM is started again.
func gradeVMIsOff(vmiPhase string) bool {
	switch vmiPhase {
	case "", "Succeeded", "Failed":
		return true
	default:
		return false
	}
}

// gradeBootGateReady reports whether a VM is ready to be graded: the VMI must
// be Running, and a VM the operator booted itself must additionally have had
// `wait` since power-on for the guest OS to bring up its serial console.
func gradeBootGateReady(vmiPhase string, autoStarted bool, bootStartedAt *metav1.Time, now time.Time, wait time.Duration) bool {
	if vmiPhase != gradeVMIPhaseRunning {
		return false
	}
	if autoStarted && bootStartedAt != nil && now.Sub(bootStartedAt.Time) < wait {
		return false
	}
	return true
}

// gradeBootTimedOut reports whether an auto-started VM has failed to reach
// Running within `timeout` of power-on.
func gradeBootTimedOut(vmiPhase string, autoStarted bool, bootStartedAt *metav1.Time, now time.Time, timeout time.Duration) bool {
	return autoStarted && bootStartedAt != nil &&
		vmiPhase != gradeVMIPhaseRunning &&
		now.Sub(bootStartedAt.Time) > timeout
}

// vmiPhase returns the KubeVirt VMI status.phase for the named VM. A missing
// VMI (the VM is stopped) yields ("", nil).
func (r *GradeRequestReconciler) vmiPhase(ctx context.Context, namespace, vmName string) (string, error) {
	obj, err := r.DynamicClient.Resource(gradeKubevirtVMIGVR).Namespace(namespace).Get(ctx, vmName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get VMI %s/%s: %w", namespace, vmName, err)
	}
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase, nil
}

func (r *GradeRequestReconciler) powerOnVM(ctx context.Context, namespace, vmName string) error {
	return r.kubevirtSubresourceAction(ctx, namespace, vmName, "start")
}

func (r *GradeRequestReconciler) powerOffVM(ctx context.Context, namespace, vmName string) error {
	return r.kubevirtSubresourceAction(ctx, namespace, vmName, "stop")
}

// kubevirtSubresourceAction calls PUT on
//
//	/apis/subresources.kubevirt.io/v1/namespaces/{namespace}/virtualmachines/{vmName}/{action}
func (r *GradeRequestReconciler) kubevirtSubresourceAction(ctx context.Context, namespace, vmName, action string) error {
	restCfg := rest.CopyConfig(r.RESTConfig)
	restCfg.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	restCfg.APIPath = "/apis"
	restCfg.NegotiatedSerializer = serializer.NewCodecFactory(runtime.NewScheme())

	restClient, err := rest.RESTClientFor(restCfg)
	if err != nil {
		return fmt.Errorf("create kubevirt rest client: %w", err)
	}

	result := restClient.Put().
		Namespace(namespace).
		Resource("virtualmachines").
		Name(vmName).
		SubResource(action).
		Body([]byte("{}")).
		Do(ctx)

	if err := result.Error(); err != nil {
		return fmt.Errorf("%s vm %s/%s: %w", action, namespace, vmName, err)
	}
	return nil
}

func (r *GradeRequestReconciler) handlePendingPhase(ctx context.Context, gr *v1alpha1.GradeRequest) error {
	log := logf.FromContext(ctx)
	normalizeGradeSpec(&gr.Spec)

	if gr.Spec.Namespace == "" {
		return r.failGradeRequest(ctx, gr, "spec.namespace is required")
	}
	if len(gr.Spec.VMs) == 0 {
		return r.failGradeRequest(ctx, gr, "spec.vms (or legacy spec.vmName) must not be empty")
	}

	gradable, reason := filterGradableVMs(gr.Spec.VMs)
	if reason != "" {
		return r.failGradeRequest(ctx, gr, reason)
	}
	gr.Spec.VMs = gradable

	// Resolve the grade method for each VM up front from the ruddervirt.io/os
	// label. Failing here turns the whole request into a clean Failed state
	// instead of scheduling jobs that are guaranteed to misbehave.
	methods := make(map[string]string, len(gr.Spec.VMs))
	for _, vm := range gr.Spec.VMs {
		method, err := r.resolveGradeMethod(ctx, gr.Spec.Namespace, vm.Name)
		if err != nil {
			return r.failGradeRequest(ctx, gr, err.Error())
		}
		methods[vm.Name] = method
	}

	// Initialize per-VM statuses once; later Pending passes must preserve the
	// boot bookkeeping (AutoStarted/BootStartedAt) stamped on earlier passes.
	if len(gr.Status.VMStatuses) != len(gr.Spec.VMs) {
		gr.Status.VMStatuses = make([]v1alpha1.GradeVMStatus, len(gr.Spec.VMs))
		for i, vm := range gr.Spec.VMs {
			gr.Status.VMStatuses[i] = v1alpha1.GradeVMStatus{Name: vm.Name, Phase: v1alpha1.GradeRequestPhasePending}
		}
	}

	needsRequeue := false
	for i, vm := range gr.Spec.VMs {
		vmStatus := &gr.Status.VMStatuses[i]
		if vmStatus.Phase != v1alpha1.GradeRequestPhasePending {
			// Already has a job or failed terminally on an earlier pass.
			continue
		}

		phase, err := r.vmiPhase(ctx, gr.Spec.Namespace, vm.Name)
		if err != nil {
			return fmt.Errorf("get VMI phase for VM %s: %w", vm.Name, err)
		}

		// Powered-off VM: boot it and grade once it has had time to come up.
		if gradeVMIsOff(phase) && vmStatus.BootStartedAt == nil {
			if err := r.powerOnVM(ctx, gr.Spec.Namespace, vm.Name); err != nil {
				vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
				vmStatus.Message = fmt.Sprintf("failed to power on VM: %v", err)
				continue
			}
			log.Info("powered on VM for grading", "gradeRequest", gr.Name, "vm", vm.Name)
			now := metav1.Now()
			vmStatus.AutoStarted = true
			vmStatus.BootStartedAt = &now
			vmStatus.Message = "Booting VM for grading"
			needsRequeue = true
			continue
		}

		if gradeBootTimedOut(phase, vmStatus.AutoStarted, vmStatus.BootStartedAt, time.Now(), gradeBootTimeout) {
			if err := r.powerOffVM(ctx, gr.Spec.Namespace, vm.Name); err != nil {
				log.Error(err, "failed to power off VM after boot timeout", "gradeRequest", gr.Name, "vm", vm.Name)
			}
			vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
			vmStatus.Message = fmt.Sprintf("VM did not reach Running within %s of power-on", gradeBootTimeout)
			vmStatus.PoweredOff = true
			continue
		}

		if !gradeBootGateReady(phase, vmStatus.AutoStarted, vmStatus.BootStartedAt, time.Now(), gradeBootWaitDuration()) {
			// A VM we did not boot that is neither off nor Running (someone
			// else is starting it, or it is stuck) must not hold the request
			// in Pending forever — give it the same window an auto-started VM
			// gets, measured from request creation.
			if !vmStatus.AutoStarted && time.Since(gr.CreationTimestamp.Time) > gradeBootTimeout {
				vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
				vmStatus.Message = fmt.Sprintf("VM not Running within %s of grade request (VMI phase %q)", gradeBootTimeout, phase)
				continue
			}
			vmStatus.Message = "Waiting for VM to be ready for grading"
			needsRequeue = true
			continue
		}

		existing, err := r.findRunningJobForVM(ctx, gr.Namespace, gr.Spec.Namespace, vm.Name)
		if err != nil {
			return fmt.Errorf("check existing jobs for VM %s: %w", vm.Name, err)
		}
		if existing != "" {
			log.Info("existing running job for VM, will requeue", "gradeRequest", gr.Name, "vm", vm.Name, "existingJob", existing)
			needsRequeue = true
			continue
		}

		job, err := r.buildJobForVM(gr, &vm, methods[vm.Name])
		if err != nil {
			return fmt.Errorf("build job for VM %s: %w", vm.Name, err)
		}
		_, err = r.Clientset.BatchV1().Jobs(gr.Namespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
			vmStatus.Message = fmt.Sprintf("failed to create job: %v", err)
			r.powerOffIfAutoStarted(ctx, gr, vmStatus)
			continue
		}

		vmStatus.Phase = v1alpha1.GradeRequestPhaseRunning
		vmStatus.JobName = job.Name
	}

	if needsRequeue {
		// Some VMs are still booting or blocked behind another grade job. Stay
		// Pending so this loop keeps driving them, and persist the boot
		// bookkeeping so it survives operator restarts.
		gr.Status.Phase = v1alpha1.GradeRequestPhasePending
		gr.Status.Message = "Waiting for VM(s) to be ready for grading"
		if err := r.updateStatus(ctx, gr); err != nil {
			return err
		}
		return errGradeRequeue
	}

	now := metav1.Now()
	gr.Status.Phase = v1alpha1.GradeRequestPhaseRunning
	gr.Status.StartedAt = &now
	gr.Status.Message = fmt.Sprintf("Created grading jobs for %d VM(s)", len(gr.Spec.VMs))
	return r.updateStatus(ctx, gr)
}

func (r *GradeRequestReconciler) handleRunningPhase(ctx context.Context, gr *v1alpha1.GradeRequest) error {
	log := logf.FromContext(ctx)
	allDone := true
	anyFailed := false
	statusChanged := false

	for i := range gr.Status.VMStatuses {
		vmStatus := &gr.Status.VMStatuses[i]

		switch vmStatus.Phase {
		case v1alpha1.GradeRequestPhaseReady:
			// Retry a power-off that failed on the terminal transition while
			// the rest of the request is still in flight.
			if r.powerOffIfAutoStarted(ctx, gr, vmStatus) {
				statusChanged = true
			}
			continue
		case v1alpha1.GradeRequestPhaseFailed:
			if r.powerOffIfAutoStarted(ctx, gr, vmStatus) {
				statusChanged = true
			}
			anyFailed = true
			continue
		case v1alpha1.GradeRequestPhasePending:
			allDone = false
			continue
		}

		// Phase is Running — check the Job
		if vmStatus.JobName == "" {
			vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
			vmStatus.Message = "job name missing from status"
			anyFailed = true
			statusChanged = true
			continue
		}

		job, err := r.Clientset.BatchV1().Jobs(gr.Namespace).Get(ctx, vmStatus.JobName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get job %s: %w", vmStatus.JobName, err)
		}

		completed := false
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				r.handleVMJobSuccess(ctx, gr, vmStatus, job)
				r.powerOffIfAutoStarted(ctx, gr, vmStatus)
				statusChanged = true
				completed = true
				break
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				r.handleVMJobFailure(ctx, gr, vmStatus, job)
				r.powerOffIfAutoStarted(ctx, gr, vmStatus)
				anyFailed = true
				statusChanged = true
				completed = true
				break
			}
		}
		if !completed {
			allDone = false
		}
	}

	if !allDone {
		if statusChanged {
			if err := r.updateStatus(ctx, gr); err != nil {
				log.Error(err, "failed to update grade request status mid-run", "gradeRequest", gr.Name, "namespace", gr.Namespace)
			}
		}
		return errGradeRequeue
	}

	// All VMs have reached a terminal state
	now := metav1.Now()
	gr.Status.CompletedAt = &now
	if anyFailed {
		gr.Status.Phase = v1alpha1.GradeRequestPhaseFailed
		gr.Status.Message = "One or more VMs failed grading"
	} else {
		gr.Status.Phase = v1alpha1.GradeRequestPhaseReady
		gr.Status.Message = "All VMs graded successfully"
	}
	return r.updateStatus(ctx, gr)
}

func (r *GradeRequestReconciler) handleVMJobSuccess(ctx context.Context, gr *v1alpha1.GradeRequest, vmStatus *v1alpha1.GradeVMStatus, job *batchv1.Job) {
	logs, err := r.readPodLogs(ctx, gr.Namespace, job.Name)
	if err != nil {
		vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
		vmStatus.Message = fmt.Sprintf("failed to read pod logs: %v", err)
		return
	}

	graderOutput := extractLastLine(logs)

	var output struct {
		Results []v1alpha1.GradeCommandResult `json:"results"`
		Error   string                        `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(graderOutput), &output); err != nil {
		vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
		vmStatus.Message = fmt.Sprintf("failed to parse grader output: %v", err)
		return
	}
	if output.Error != "" {
		vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
		vmStatus.Message = output.Error
		return
	}

	vmStatus.Phase = v1alpha1.GradeRequestPhaseReady
	vmStatus.Results = output.Results
}

func (r *GradeRequestReconciler) handleVMJobFailure(ctx context.Context, gr *v1alpha1.GradeRequest, vmStatus *v1alpha1.GradeVMStatus, job *batchv1.Job) {
	logs, err := r.readPodLogs(ctx, gr.Namespace, job.Name)
	if err != nil {
		logs = fmt.Sprintf("failed to read logs: %v", err)
	}

	msg := "Job failed"
	lastLine := extractLastLine(logs)
	var output struct {
		Error string `json:"error,omitempty"`
	}
	if json.Unmarshal([]byte(lastLine), &output) == nil && output.Error != "" {
		msg = output.Error
	} else if lastLine != "" {
		msg = lastLine
	}

	vmStatus.Phase = v1alpha1.GradeRequestPhaseFailed
	vmStatus.Message = msg
}

// powerOffIfAutoStarted powers a VM back off after grading iff the operator
// booted it for this grade request; VMs that were already running are left
// running. Returns true when the status changed. Power-off failures are
// logged, never propagated: the grading result stands on its own, and the
// Ready/Failed arms of handleRunningPhase retry while the request is in
// flight.
func (r *GradeRequestReconciler) powerOffIfAutoStarted(ctx context.Context, gr *v1alpha1.GradeRequest, vmStatus *v1alpha1.GradeVMStatus) bool {
	log := logf.FromContext(ctx)
	if !vmStatus.AutoStarted || vmStatus.PoweredOff {
		return false
	}
	// Another grade request may have an active job against this VM; powering
	// it off would kill that grade mid-flight. Leave the VM running.
	if other, err := r.findRunningJobForVM(ctx, gr.Namespace, gr.Spec.Namespace, vmStatus.Name); err == nil && other != "" && other != vmStatus.JobName {
		log.Info("leaving auto-started VM running: another grade job is active", "gradeRequest", gr.Name, "vm", vmStatus.Name, "activeJob", other)
		vmStatus.PoweredOff = true
		return true
	}
	if err := r.powerOffVM(ctx, gr.Spec.Namespace, vmStatus.Name); err != nil {
		log.Error(err, "failed to power off auto-started VM after grading", "gradeRequest", gr.Name, "vm", vmStatus.Name)
		return false
	}
	log.Info("powered off auto-started VM after grading", "gradeRequest", gr.Name, "vm", vmStatus.Name)
	vmStatus.PoweredOff = true
	return true
}

func (r *GradeRequestReconciler) buildJobForVM(gr *v1alpha1.GradeRequest, vm *v1alpha1.GradeVM, method string) (*batchv1.Job, error) {
	commandsJSON, _ := json.Marshal(vm.Commands)

	activeDeadline := gradeJobActiveDeadline
	backoffLimit := gradeJobBackoffLimit
	ttl := int32(3600)

	var pullSecrets []corev1.LocalObjectReference
	if names := os.Getenv("IMAGE_PULL_SECRETS"); names != "" {
		for name := range strings.SplitSeq(names, ",") {
			name = strings.TrimSpace(name)
			if name != "" {
				pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
			}
		}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			// gr.Name already starts with "grade-"; appending an 8-char hash of
			// vm.Name keeps the job name unique per VM while staying well under
			// the 63-char cap that Kubernetes enforces on the auto-injected
			// job-name pod label.
			Name:      fmt.Sprintf("%s-%s", gr.Name, shortVMHash(vm.Name)),
			Namespace: gr.Namespace,
			Labels: map[string]string{
				v1alpha1.LabelGradeRequest:  gr.Name,
				v1alpha1.LabelGradeTargetVM: vm.Name,
				v1alpha1.LabelGradeTargetNS: gr.Spec.Namespace,
			},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &activeDeadline,
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelGradeRequest:  gr.Name,
						v1alpha1.LabelGradeTargetVM: vm.Name,
						v1alpha1.LabelGradeTargetNS: gr.Spec.Namespace,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: gradeServiceAccountName,
					ImagePullSecrets:   pullSecrets,
					Containers: []corev1.Container{
						{
							Name:            "grader",
							Image:           gradeGraderImage(),
							ImagePullPolicy: corev1.PullAlways,
							Env: []corev1.EnvVar{
								{Name: "GRADE_NAMESPACE", Value: gr.Spec.Namespace},
								{Name: "GRADE_VM_NAME", Value: vm.Name},
								{Name: "GRADE_COMMANDS", Value: string(commandsJSON)},
								{Name: "GRADE_USER", Value: vm.User},
								{Name: "GRADE_PASSWORD", Value: vm.Password},
								{Name: "GRADE_DOMAIN", Value: vm.Domain},
								{Name: "GRADE_METHOD", Value: method},
							},
						},
					},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(gr, job, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference: %w", err)
	}
	return job, nil
}

func shortVMHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:4])
}

func extractLastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func (r *GradeRequestReconciler) findRunningJobForVM(ctx context.Context, namespace, targetNamespace, targetVM string) (string, error) {
	jobs, err := r.Clientset.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", v1alpha1.LabelGradeTargetVM, targetVM, v1alpha1.LabelGradeTargetNS, targetNamespace),
	})
	if err != nil {
		return "", err
	}

	for _, job := range jobs.Items {
		if job.Status.Active > 0 {
			return job.Name, nil
		}
	}
	return "", nil
}

func (r *GradeRequestReconciler) readPodLogs(ctx context.Context, namespace, jobName string) (string, error) {
	pods, err := r.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil {
		return "", fmt.Errorf("list pods for job %s: %w", jobName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s", jobName)
	}

	podName := pods.Items[0].Name
	logStream, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("get logs for pod %s: %w", podName, err)
	}
	defer func() { _ = logStream.Close() }()

	data, err := io.ReadAll(logStream)
	if err != nil {
		return "", fmt.Errorf("read logs for pod %s: %w", podName, err)
	}
	return string(data), nil
}

func (r *GradeRequestReconciler) updateStatus(ctx context.Context, gr *v1alpha1.GradeRequest) error {
	if err := r.Status().Update(ctx, gr); err != nil {
		return fmt.Errorf("update grade request status: %w", err)
	}
	return nil
}

func (r *GradeRequestReconciler) cleanupIfExpired(ctx context.Context, gr *v1alpha1.GradeRequest) error {
	log := logf.FromContext(ctx)
	if gr.Status.CompletedAt == nil {
		return nil
	}
	if time.Since(gr.Status.CompletedAt.Time) < gradeCompletedRetention {
		return nil
	}

	log.Info("cleaning up completed grade request", "gradeRequest", gr.Name, "namespace", gr.Namespace)

	if err := r.Delete(ctx, gr); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete completed grade request %s/%s: %w", gr.Namespace, gr.Name, err)
	}
	return nil
}

func (r *GradeRequestReconciler) failGradeRequest(ctx context.Context, gr *v1alpha1.GradeRequest, msg string) error {
	log := logf.FromContext(ctx)
	now := metav1.Now()
	gr.Status.Phase = v1alpha1.GradeRequestPhaseFailed
	gr.Status.Message = msg
	gr.Status.CompletedAt = &now
	if err := r.updateStatus(ctx, gr); err != nil {
		log.Error(err, "failed to update grade request status to Failed", "gradeRequest", gr.Name, "namespace", gr.Namespace)
		return err
	}
	return nil
}

func (r *GradeRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	dynamicClient, err := dynamic.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}
	r.DynamicClient = dynamicClient
	r.Clientset = clientset

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GradeRequest{}).
		Owns(&batchv1.Job{}).
		Named("graderequest").
		WithOptions(ctrlcontroller.Options{MaxConcurrentReconciles: gradeMaxConcurrentReconc}).
		Complete(r)
}
