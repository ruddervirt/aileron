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

		// Only Pending orphans that no VM references are deleted. A Bound orphan may
		// hold a real VM disk, and a referenced PVC (efivars via the hook sidecar, or
		// a data volume) is still in use — leave both in place.
		if pvc.Status.Phase != corev1.ClaimPending || referencedPVCs[pvc.Name] {
			remainingByClone[cloneID]++
			continue
		}

		if err := r.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "deleting orphaned clone PVC", "pvc", pvc.Name)
			remainingByClone[cloneID]++
			continue
		}
		orphanClonePVCsDeleted.Inc()
		logger.Info("Deleted orphaned Pending clone PVC", "pvc", pvc.Name, "cloneID", cloneID)
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

	// Clean up orphaned clone VirtualMachineNamespace roots, but only once no clone
	// PVCs remain for that ID (so deleting the VMNS never cascade-deletes an owned
	// PVC that survived above).
	return r.reapOrphanedVMNS(ctx, ns, isLive, remainingByClone)
}

// liveCloneCRIDs returns the set of CloneIDs for VirtualMachineClones that still exist
// (an in-progress clone whose VMs are not created yet still owns its Pending PVCs).
func (r *PVCReaper) liveCloneCRIDs(ctx context.Context) (map[string]struct{}, error) {
	clones := &v1alpha1.VirtualMachineCloneList{}
	if err := r.Reader.List(ctx, clones); err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(clones.Items))
	for i := range clones.Items {
		if id := clones.Items[i].Status.CloneID; id != "" {
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

// reapOrphanedVMNS deletes clone-owned VirtualMachineNamespace roots whose clone is
// dead and whose clone PVCs are all gone. Build-owned VMNS CRs are never touched.
func (r *PVCReaper) reapOrphanedVMNS(ctx context.Context, ns string, isLive func(string) bool, remainingByClone map[string]int) error {
	logger := log.FromContext(ctx).WithName("pvc-reaper")

	vmnsList := &v1alpha1.VirtualMachineNamespaceList{}
	if err := r.Reader.List(ctx, vmnsList, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range vmnsList.Items {
		vmns := &vmnsList.Items[i]
		if vmns.Spec.OwnerRef == nil || vmns.Spec.OwnerRef.Kind != "VirtualMachineClone" {
			continue
		}
		cloneID := vmns.Name
		if isLive(cloneID) {
			continue
		}
		// Wait until no clone PVCs remain for this ID before removing the owner root.
		if remainingByClone[cloneID] > 0 {
			continue
		}
		if err := r.Client.Delete(ctx, vmns); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "deleting orphaned VirtualMachineNamespace", "name", cloneID)
			continue
		}
		logger.Info("Deleted orphaned VirtualMachineNamespace", "name", cloneID)
	}
	return nil
}
