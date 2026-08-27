// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const cloneLabel = "ruddervirt.io/clone"

// Default reaper cadence and the age past which a Pending clone PVC is reported as
// stuck. Kept short so a dead-source PVC never lingers for more than a few minutes.
const (
	defaultReapInterval     = 5 * time.Minute
	defaultReapPendingAfter = 5 * time.Minute
)

// boundOrphanGrace is the buffer required past a Bound orphan PVC's own
// ruddervirt.io/expires-at annotation before it is treated as safe to delete —
// guards against acting on a clone that is still mid-teardown through the normal
// (non-force-delete) path.
const boundOrphanGrace = 1 * time.Hour

var (
	// pendingClonePVCs gauges clone PVCs that have been Pending past the threshold —
	// the alerting signal for "PVC stuck Pending for over N minutes".
	pendingClonePVCs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aileron_clone_pvcs_pending",
		Help: "Number of clone PVCs that have been Pending longer than the reaper threshold.",
	})
	// orphanedClonePVCs gauges clone PVCs whose owning VirtualMachineClone no longer exists.
	orphanedClonePVCs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "aileron_clone_pvcs_orphaned",
		Help: "Number of clone PVCs whose owning VirtualMachineClone no longer exists.",
	})
	// orphanClonePVCsDeleted counts clone PVCs the reaper has garbage-collected.
	orphanClonePVCsDeleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "aileron_clone_pvcs_deleted_total",
		Help: "Total clone PVCs deleted by the reaper because their VirtualMachineClone was gone.",
	})
)

func init() {
	crmetrics.Registry.MustRegister(pendingClonePVCs, orphanedClonePVCs, orphanClonePVCsDeleted)
}

// PVCReaper is a background manager.Runnable that keeps clone storage from leaking.
// Once a VirtualMachineClone is deleted (including a force-delete that skips the
// finalizer sweep), its clone PVCs are orphaned; nothing else garbage-collects them,
// and a Pending orphan can even statically adopt an unrelated Available PV. The
// reaper periodically deletes clone PVCs whose owning clone is gone and which no
// live VM uses, and exposes metrics for Pending/orphaned PVCs. It also sweeps
// expired/stalled cached ISO DataVolumes (see ISOImporter.CleanupExpiredISOs) so a
// dead-URL import can't crashloop indefinitely between builds.
type PVCReaper struct {
	// Client issues deletes.
	Client client.Client
	// Reader is an uncached reader for the periodic lists so the reaper does not
	// spin up cluster-wide informers (e.g. over every KubeVirt VM).
	Reader client.Reader
	// Interval is the sweep cadence (defaults to defaultReapInterval).
	Interval time.Duration
	// PendingThreshold is the age past which a Pending clone PVC is counted as stuck
	// (defaults to defaultReapPendingAfter).
	PendingThreshold time.Duration
}

// NeedLeaderElection ensures only the elected leader runs the reaper when leader
// election is enabled, so replicas do not race on deletes.
func (r *PVCReaper) NeedLeaderElection() bool { return true }

// Start runs the sweep loop until the context is cancelled. It satisfies
// sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
func (r *PVCReaper) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("pvc-reaper")
	interval := r.Interval
	if interval <= 0 {
		interval = defaultReapInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Sweep once at startup so the backlog is addressed without waiting a full tick.
	if err := r.sweep(ctx); err != nil {
		logger.Error(err, "initial PVC reaper sweep failed")
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.sweep(ctx); err != nil {
				logger.Error(err, "PVC reaper sweep failed")
			}
		}
	}
}

// sweep garbage-collects orphaned clone storage and updates metrics. A clone is
// "dead" only when it has neither a live VirtualMachineClone CR (protects in-progress
// clones) nor any live VM carrying its ruddervirt.io/clone label (protects completed
// clones whose CR was force-deleted). The reaper deletes only PENDING orphans that no
// VM references — a Pending PVC holds no committed data and is never mounted, so this
// is safe on a busy cluster. Bound orphans (leaked disks holding real data) are
// surfaced via a metric but never auto-deleted.
func (r *PVCReaper) sweep(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("pvc-reaper")
	ns := OperatorNamespace()
	threshold := r.PendingThreshold
	if threshold <= 0 {
		threshold = defaultReapPendingAfter
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.Reader.List(ctx, pvcs, client.InNamespace(ns), client.HasLabels{cloneLabel}); err != nil {
		return err
	}

	cloneCRIDs, err := r.liveCloneCRIDs(ctx)
	if err != nil {
		return err
	}
	vmCloneIDs, referencedPVCs, err := r.vmState(ctx, ns)
	if err != nil {
		return err
	}
	isLive := func(cloneID string) bool {
		if _, ok := cloneCRIDs[cloneID]; ok {
			return true
		}
		_, ok := vmCloneIDs[cloneID]
		return ok
	}

	var pending, orphaned int
	remainingByClone := make(map[string]int)
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		cloneID := pvc.Labels[cloneLabel]

		stuckPending := pvc.Status.Phase == corev1.ClaimPending &&
			!pvc.CreationTimestamp.IsZero() &&
			time.Since(pvc.CreationTimestamp.Time) > threshold
		if stuckPending {
			pending++
		}

		if isLive(cloneID) {
			remainingByClone[cloneID]++
			continue
		}
		orphaned++

		// A referenced PVC (efivars via the hook sidecar, or a data volume) is still in
		// use by some VM regardless of its own clone's liveness — never delete it.
		if referencedPVCs[pvc.Name] {
			remainingByClone[cloneID]++
			continue
		}

		// Pending orphans are always safe to delete: they hold no committed data and
		// are never mounted. Bound orphans may hold a real VM disk, so they are only
		// deleted when the PVC itself carries a ruddervirt.io/expires-at annotation
		// (stamped at creation, see EnsureClonePVC) that safely passed — that is the
		// one signal proving the clone is actually done, not mid-teardown. A Bound
		// orphan with no stamped expiry (pre-dating that annotation, or any other
		// edge case) is left in place and only surfaced via the metric below.
		safeToDelete := pvc.Status.Phase == corev1.ClaimPending ||
			(pvc.Status.Phase == corev1.ClaimBound && pvcExpiredBefore(pvc, time.Now().Add(-boundOrphanGrace)))
		if !safeToDelete {
			remainingByClone[cloneID]++
			continue
		}

		if err := r.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "deleting orphaned clone PVC", "pvc", pvc.Name)
			remainingByClone[cloneID]++
			continue
		}
		orphanClonePVCsDeleted.Inc()
		logger.Info("Deleted orphaned clone PVC", "pvc", pvc.Name, "cloneID", cloneID, "phase", pvc.Status.Phase)
	}

	pendingClonePVCs.Set(float64(pending))
	orphanedClonePVCs.Set(float64(orphaned))

	// Also reap expired and stalled ISO caches. This is the periodic backstop:
	// the build controller only runs ISO cleanup when some build reaches a
	// terminal phase, so without it a dead-URL import (crashlooping importer
	// pod + prime PVC) lingers until the next build finishes — or forever on
	// a quiet cluster. Best-effort: an ISO sweep failure must not stop the
	// clone-PVC sweep below.
	isoImporter := &build.ISOImporter{Client: r.Client, OperatorNS: ns}
	if err := isoImporter.CleanupExpiredISOs(ctx, ns, build.DefaultISOCacheTTL); err != nil {
		logger.Error(err, "ISO cache sweep failed")
	}

	// Same backstop for cached boot-disk sources (ruddervirt.io/source-cache):
	// a dead-URL/unreachable source cache DV crashloops its importer-prime pod
	// forever otherwise — see CleanupExpiredSourceCaches.
	sourceImporter := &build.SourceImporter{Client: r.Client, OperatorNS: ns}
	if err := sourceImporter.CleanupExpiredSourceCaches(ctx, ns, build.DefaultSourceCacheTTL); err != nil {
		logger.Error(err, "source cache sweep failed")
	}

	// Clean up orphaned clone VirtualMachineNamespace roots, but only once no clone
	// PVCs remain for that ID (so deleting the VMNS never cascade-deletes an owned
	// PVC that survived above).
	return r.reapOrphanedVMNS(ctx, ns, isLive, remainingByClone)
}

// pvcExpiredBefore reports whether pvc carries a ruddervirt.io/expires-at annotation
// (stamped at creation time by EnsureClonePVC) that parses and falls before cutoff. A
// missing or unparseable annotation is treated as "not expired" — the conservative
// answer, since it means there is no PVC-local signal proving the clone is done.
func pvcExpiredBefore(pvc *corev1.PersistentVolumeClaim, cutoff time.Time) bool {
	raw, ok := pvc.Annotations[v1alpha1.AnnotationExpiresAt]
	if !ok {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return expiresAt.Before(cutoff)
}

// liveCloneCRIDs returns the set of CloneIDs for VirtualMachineClones that are still
// in progress — CompletionTime unset — and so still own their (Pending) PVCs before
// any VM exists to carry the clone label.
//
// A completed clone (CompletionTime set) is deliberately excluded even though its CR
// still exists: nothing in this repo ever deletes a VirtualMachineClone CR once it
// completes (that is left to whatever created it), so on this cluster completed CRs
// accumulate indefinitely — treating CR-existence as blanket liveness forever would
// mean a clone whose VMs were later deleted (external watchdog, expiry, ...) keeps
// "protecting" its now-orphaned PVCs forever too, with no way for this reaper to ever
// reclaim them. By the time CompletionTime is stamped the clone's VMs already exist
// and carry the ruddervirt.io/clone label (EnsureVMs runs well before completion), so
// vmCloneIDs alone is the authoritative liveness signal from that point on — matching
// how this cluster's clone CRs are actually ephemeral in practice: the VM label, not
// the CR, is what should be trusted once provisioning is done.
func (r *PVCReaper) liveCloneCRIDs(ctx context.Context) (map[string]struct{}, error) {
	clones := &v1alpha1.VirtualMachineCloneList{}
	if err := r.Reader.List(ctx, clones); err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(clones.Items))
	for i := range clones.Items {
		c := &clones.Items[i]
		if c.Status.CompletionTime != nil {
			continue
		}
		if id := c.Status.CloneID; id != "" {
			live[id] = struct{}{}
		}
	}
	return live, nil
}

// vmState scans KubeVirt VMs in the namespace and returns (a) the set of clone IDs
// that still have at least one VM (from the ruddervirt.io/clone label) and (b) the set
// of PVC names any VM references — both as a volume's persistentVolumeClaim.claimName
// and as an EFI-vars PVC attached through the hooks.kubevirt.io/hookSidecars
// annotation (which does NOT appear in the volume list). Missing either would let the
// reaper delete storage a live VM depends on.
func (r *PVCReaper) vmState(ctx context.Context, ns string) (cloneIDs map[string]struct{}, referenced map[string]bool, err error) {
	vms := &unstructured.UnstructuredList{}
	vms.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList",
	})
	if err := r.Reader.List(ctx, vms, client.InNamespace(ns)); err != nil {
		return nil, nil, err
	}
	cloneIDs = make(map[string]struct{})
	referenced = make(map[string]bool)
	for i := range vms.Items {
		vm := &vms.Items[i]
		if cid := vm.GetLabels()[cloneLabel]; cid != "" {
			cloneIDs[cid] = struct{}{}
		}
		volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
		for _, vol := range volumes {
			volMap, ok := vol.(map[string]any)
			if !ok {
				continue
			}
			if pvc, ok := volMap["persistentVolumeClaim"].(map[string]any); ok {
				if name, _, _ := unstructured.NestedString(pvc, "claimName"); name != "" {
					referenced[name] = true
				}
			}
		}
		// EFI-vars PVC(s) referenced only through the hook sidecar annotation.
		hook, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "metadata", "annotations", "hooks.kubevirt.io/hookSidecars")
		if hook != "" {
			var sidecars []struct {
				PVC struct {
					Name string `json:"name"`
				} `json:"pvc"`
			}
			if json.Unmarshal([]byte(hook), &sidecars) == nil {
				for _, s := range sidecars {
					if s.PVC.Name != "" {
						referenced[s.PVC.Name] = true
					}
				}
			}
		}
	}
	return cloneIDs, referenced, nil
}

// liveBuildNames returns the set of "namespace/name" for VirtualMachineBuild CRs that
// still exist, used to tell whether a build-owned VirtualMachineNamespace root
// (reapOrphanedVMNS) is orphaned.
func (r *PVCReaper) liveBuildNames(ctx context.Context) (map[string]struct{}, error) {
	builds := &v1alpha1.VirtualMachineBuildList{}
	if err := r.Reader.List(ctx, builds); err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(builds.Items))
	for i := range builds.Items {
		b := &builds.Items[i]
		live[b.Namespace+"/"+b.Name] = struct{}{}
	}
	return live, nil
}

// reapOrphanedVMNS deletes VirtualMachineNamespace roots whose owner is gone:
//   - clone-owned (OwnerRef.Kind == "VirtualMachineClone"): once the clone is dead
//     (isLive) and all its clone PVCs are gone (remainingByClone), matching how
//     EnsureClonePVC owns clone PVCs by the VMNS — deleting it early would cascade
//     an owned PVC that survived the sweep above.
//   - build-owned (OwnerRef.Kind == "VirtualMachineBuild"): once the named build CR
//     no longer exists. No build resource is owned by the VMNS root (efivars PVCs are
//     owned directly by their VM — see ownEFIPVCByVM), so there is nothing to cascade
//     and no remaining-count gate is needed. Previously nothing reaped these at all:
//     the only other cleanup path is the VirtualMachineBuild controller's own
//     handleDeletion, whose VMNS-delete error is only logged, never retried, before
//     the finalizer is removed regardless — so a transient failure there leaked the
//     VMNS permanently.
func (r *PVCReaper) reapOrphanedVMNS(ctx context.Context, ns string, isLive func(string) bool, remainingByClone map[string]int) error {
	logger := log.FromContext(ctx).WithName("pvc-reaper")

	liveBuilds, err := r.liveBuildNames(ctx)
	if err != nil {
		return err
	}

	vmnsList := &v1alpha1.VirtualMachineNamespaceList{}
	if err := r.Reader.List(ctx, vmnsList, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range vmnsList.Items {
		vmns := &vmnsList.Items[i]
		if vmns.Spec.OwnerRef == nil {
			continue
		}

		switch vmns.Spec.OwnerRef.Kind {
		case "VirtualMachineClone":
			cloneID := vmns.Name
			if isLive(cloneID) {
				continue
			}
			// Wait until no clone PVCs remain for this ID before removing the owner root.
			if remainingByClone[cloneID] > 0 {
				continue
			}
		case "VirtualMachineBuild":
			key := vmns.Spec.OwnerRef.Namespace + "/" + vmns.Spec.OwnerRef.Name
			if _, ok := liveBuilds[key]; ok {
				continue
			}
		default:
			continue
		}

		if err := r.Client.Delete(ctx, vmns); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "deleting orphaned VirtualMachineNamespace", "name", vmns.Name, "ownerKind", vmns.Spec.OwnerRef.Kind)
			continue
		}
		logger.Info("Deleted orphaned VirtualMachineNamespace", "name", vmns.Name, "ownerKind", vmns.Spec.OwnerRef.Kind)
	}
	return nil
}
