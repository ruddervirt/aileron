// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// defaultStatsInterval is the collection cadence used when StatsCollector.Interval
// is unset. Fast enough that phase-count gauges feel close to real-time on a
// dashboard, cheap enough to run against the manager's own warm CR caches.
const defaultStatsInterval = 30 * time.Second

var (
	// buildCount gauges the current number of VirtualMachineBuild CRs by phase.
	buildCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aileron_build_count",
		Help: "Current number of VirtualMachineBuild CRs by phase.",
	}, []string{"phase"})
	// cloneCount gauges the current number of VirtualMachineClone CRs by phase.
	cloneCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aileron_clone_count",
		Help: "Current number of VirtualMachineClone CRs by phase.",
	}, []string{"phase"})
	// gradeReqCount gauges the current number of GradeRequest CRs by phase.
	gradeReqCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aileron_gradereq_count",
		Help: "Current number of GradeRequest CRs by phase.",
	}, []string{"phase"})
	// vmnsCount gauges the current number of VirtualMachineNamespace CRs by phase.
	vmnsCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aileron_vmns_count",
		Help: "Current number of VirtualMachineNamespace CRs by phase.",
	}, []string{"phase"})

	// gradeReqActiveSlots gauges grading concurrency slots currently in use.
	gradeReqActiveSlots = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aileron_gradereq_active_slots",
		Help: "Grading concurrency slots currently in use, across live GradeRequests.",
	})
	// gradeReqMaxSlots gauges the configured grading concurrency limit.
	gradeReqMaxSlots = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aileron_gradereq_max_slots",
		Help: "Configured grading concurrency limit (0 = unlimited).",
	})
	// gradeReqQueued gauges VMs queued waiting for a grading slot.
	gradeReqQueued = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aileron_gradereq_queued",
		Help: "Total VMs queued across live GradeRequests waiting for a grading slot.",
	})

	// podErrors gauges Aileron-managed pods currently in an error state, by reason.
	podErrors = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aileron_pod_errors",
		Help: "Current count of Aileron-managed pods in an error state, by reason.",
	}, []string{"reason"})

	// pvcBytes gauges Aileron-managed PVC storage size in bytes, by type and owner.
	pvcBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "aileron_pvc_bytes",
		Help: "Aileron-managed PVC storage size in bytes, by type (requested|capacity) and owner (build|clone).",
	}, []string{"type", "owner"})
)

func init() {
	crmetrics.Registry.MustRegister(buildCount, cloneCount, gradeReqCount, vmnsCount,
		gradeReqActiveSlots, gradeReqMaxSlots, gradeReqQueued, podErrors, pvcBytes)
}

// StatsCollector is a background manager.Runnable that periodically republishes
// build/clone/grade/pod/PVC counts as Prometheus gauges.
type StatsCollector struct {
	// Client is the manager's cached client, used to list Aileron's own CRs —
	// each already has a warm informer from its own controller's watch.
	Client client.Client
	// Reader is an uncached reader for Pods and PVCs, so the collector never
	// caches full pod/PVC objects for data it only summarizes.
	Reader client.Reader
	// Interval is the collection cadence (defaults to defaultStatsInterval).
	Interval time.Duration
}

// NeedLeaderElection ensures only the elected leader collects stats when leader
// election is enabled, so replicas do not publish duplicate/conflicting series.
func (s *StatsCollector) NeedLeaderElection() bool { return true }

// Start runs the collection loop until the context is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (s *StatsCollector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("stats-collector")
	interval := s.Interval
	if interval <= 0 {
		interval = defaultStatsInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := s.collect(ctx); err != nil {
		logger.Error(err, "initial stats collection failed")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.collect(ctx); err != nil {
				logger.Error(err, "stats collection failed")
			}
		}
	}
}

// collect re-derives every gauge from scratch each tick, so a phase or reason
// that drops to zero is reported as 0 rather than left stale at its last value.
func (s *StatsCollector) collect(ctx context.Context) error {
	ns := OperatorNamespace()

	if err := s.collectBuilds(ctx); err != nil {
		return err
	}
	if err := s.collectClones(ctx); err != nil {
		return err
	}
	if err := s.collectGradeRequests(ctx); err != nil {
		return err
	}
	if err := s.collectVMNamespaces(ctx, ns); err != nil {
		return err
	}
	if err := s.collectPVCs(ctx, ns); err != nil {
		return err
	}
	return s.collectPodErrors(ctx, ns)
}

func (s *StatsCollector) collectBuilds(ctx context.Context) error {
	list := &v1alpha1.VirtualMachineBuildList{}
	if err := s.Client.List(ctx, list); err != nil {
		return err
	}
	counts := map[string]float64{}
	for i := range list.Items {
		counts[string(list.Items[i].Status.Phase)]++
	}
	buildCount.Reset()
	for phase, n := range counts {
		buildCount.WithLabelValues(phase).Set(n)
	}
	return nil
}

func (s *StatsCollector) collectClones(ctx context.Context) error {
	list := &v1alpha1.VirtualMachineCloneList{}
	if err := s.Client.List(ctx, list); err != nil {
		return err
	}
	counts := map[string]float64{}
	for i := range list.Items {
		counts[string(list.Items[i].Status.Phase)]++
	}
	cloneCount.Reset()
	for phase, n := range counts {
		cloneCount.WithLabelValues(phase).Set(n)
	}
	return nil
}

// collectGradeRequests updates the phase-count gauge and the grading-concurrency
// gauges. ActiveSlots/MaxSlots/QueuedCount are already computed and written into
// every non-terminal GradeRequest's own Status by the concurrency gate each
// reconcile — this only aggregates that existing data, taking the max of
// ActiveSlots/MaxSlots and the sum of QueuedCount across non-terminal requests.
func (s *StatsCollector) collectGradeRequests(ctx context.Context) error {
	list := &v1alpha1.GradeRequestList{}
	if err := s.Client.List(ctx, list); err != nil {
		return err
	}

	counts := map[string]float64{}
	var activeSlots, maxSlots, queued int32
	for i := range list.Items {
		gr := &list.Items[i]
		counts[string(gr.Status.Phase)]++
		if gr.Status.Phase == v1alpha1.GradeRequestPhaseReady || gr.Status.Phase == v1alpha1.GradeRequestPhaseFailed {
			continue
		}
		if gr.Status.ActiveSlots > activeSlots {
			activeSlots = gr.Status.ActiveSlots
		}
		if gr.Status.MaxSlots > maxSlots {
			maxSlots = gr.Status.MaxSlots
		}
		queued += gr.Status.QueuedCount
	}

	gradeReqCount.Reset()
	for phase, n := range counts {
		gradeReqCount.WithLabelValues(phase).Set(n)
	}
	gradeReqActiveSlots.Set(float64(activeSlots))
	gradeReqMaxSlots.Set(float64(maxSlots))
	gradeReqQueued.Set(float64(queued))
	return nil
}

func (s *StatsCollector) collectVMNamespaces(ctx context.Context, ns string) error {
	list := &v1alpha1.VirtualMachineNamespaceList{}
	if err := s.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return err
	}
	counts := map[string]float64{}
	for i := range list.Items {
		counts[string(list.Items[i].Status.Phase)]++
	}
	vmnsCount.Reset()
	for phase, n := range counts {
		vmnsCount.WithLabelValues(phase).Set(n)
	}
	return nil
}

// collectPVCs gauges requested/capacity storage bytes for every build- or
// clone-owned PVC. Deliberately a separate, unfiltered list rather than reusing
// PVCReaper's clone-only PVC list (see pvc_reaper.go's cloneLabel usage), which
// would silently omit every build disk PVC.
func (s *StatsCollector) collectPVCs(ctx context.Context, ns string) error {
	list := &corev1.PersistentVolumeClaimList{}
	if err := s.Reader.List(ctx, list, client.InNamespace(ns)); err != nil {
		return err
	}

	sums := map[[2]string]float64{} // [type, owner] -> bytes
	for i := range list.Items {
		pvc := &list.Items[i]
		var owner string
		switch {
		case pvc.Labels[build.LabelBuildID] != "":
			owner = "build"
		case pvc.Labels[cloneLabel] != "":
			owner = "clone"
		default:
			continue
		}
		if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			sums[[2]string{"requested", owner}] += float64(q.Value())
		}
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			sums[[2]string{"capacity", owner}] += float64(q.Value())
		}
	}

	pvcBytes.Reset()
	for k, v := range sums {
		pvcBytes.WithLabelValues(k[0], k[1]).Set(v)
	}
	return nil
}

// podErrorReasons are the container waiting/terminated reasons counted as pod
// errors, matching the vocabulary already checked by build.VMBooter's
// checkLauncherPod (internal/build/vm.go) plus terminated-state reasons it
// doesn't need to check for its narrower single-VM lookup.
var podErrorReasons = map[string]struct{}{
	"ImagePullBackOff": {},
	"ErrImagePull":     {},
	"InvalidImageName": {},
	"CrashLoopBackOff": {},
	"OOMKilled":        {},
	"Error":            {},
}

// collectPodErrors gauges Aileron-managed pods currently in an error state, by
// reason. "Aileron-managed" is determined by the same labels used elsewhere to
// identify build/clone/grade resources (build.LabelBuildID, cloneLabel,
// v1alpha1.LabelGradeRequest).
func (s *StatsCollector) collectPodErrors(ctx context.Context, ns string) error {
	counts := map[string]float64{}
	for _, label := range []string{build.LabelBuildID, cloneLabel, v1alpha1.LabelGradeRequest} {
		list := &corev1.PodList{}
		if err := s.Reader.List(ctx, list, client.InNamespace(ns), client.HasLabels{label}); err != nil {
			return err
		}
		for i := range list.Items {
			countPodErrors(&list.Items[i], counts)
		}
	}

	podErrors.Reset()
	for reason, n := range counts {
		podErrors.WithLabelValues(reason).Set(n)
	}
	return nil
}

// countPodErrors tallies one error reason per pod into counts: the pod phase if
// Failed, otherwise the first matching container waiting/terminated reason.
func countPodErrors(pod *corev1.Pod, counts map[string]float64) {
	if pod.Status.Phase == corev1.PodFailed {
		counts["Failed"]++
		return
	}
	allStatuses := append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...)
	for _, cs := range allStatuses {
		if cs.State.Waiting != nil {
			if _, ok := podErrorReasons[cs.State.Waiting.Reason]; ok {
				counts[cs.State.Waiting.Reason]++
				return
			}
		}
		if cs.State.Terminated != nil {
			if _, ok := podErrorReasons[cs.State.Terminated.Reason]; ok {
				counts[cs.State.Terminated.Reason]++
				return
			}
		}
	}
}
