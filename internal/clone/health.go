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

package clone

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LabelBaseSnapshot and LabelSourcePVC identify a base VolumeSnapshot created
// by EnsureBaseSnapshotReady (snapshot.go) for a given source PVC.
const (
	LabelBaseSnapshot = "ruddervirt.io/base-snapshot"
	LabelSourcePVC    = "ruddervirt.io/source-pvc"
)

var volumeSnapshotListGVK = schema.GroupVersionKind{
	Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList",
}

// CheckTemplateHealth reports whether templateName's template can currently
// back a clone. It is strictly read-only: unlike EnsureBaseSnapshotReady, it
// never creates a VolumeSnapshot or any other resource — a health check that
// manufactures the state it claims to observe would be worthless.
//
// A non-nil error return means the check itself could not complete (an
// infrastructure problem: RBAC, timeout, ...) — callers must not persist the
// zero-value TemplateHealth in that case. A nil error always carries an
// authoritative verdict, Clonable true or false.
func CheckTemplateHealth(ctx context.Context, c client.Client, buildNamespace, templateName string) (v1alpha1.TemplateHealth, error) {
	now := metav1.Now()

	templateNS, vms, err := ValidateTemplate(ctx, c, buildNamespace, templateName)
	if err != nil {
		// ValidateTemplate's own contract is that every failure path already
		// returns an actionable diagnosis (see its doc comment) — the existing
		// clone controller (handleValidating, virtualmachineclone_controller.go)
		// already treats any error from it as terminal, not transient. Follow
		// that precedent: this is a diagnosis, not an infra failure.
		return v1alpha1.TemplateHealth{
			Clonable:  false,
			Missing:   []string{"template-vm/" + templateName},
			Message:   err.Error(),
			CheckedAt: now,
		}, nil
	}

	missing, message, err := walkTemplateStorage(ctx, c, templateNS, templateName, vms)
	if err != nil {
		return v1alpha1.TemplateHealth{}, fmt.Errorf("checking template storage for %s: %w", templateName, err)
	}

	return v1alpha1.TemplateHealth{
		Clonable:  len(missing) == 0,
		Missing:   missing,
		Message:   message,
		CheckedAt: now,
	}, nil
}

// walkTemplateStorage performs the read-only PVC/PV/snapshot walk, mirroring
// SnapshotManager.BuildInitialVolumeStates's resolution logic but with
// different error semantics: every problem is accumulated into missing
// (scanning continues) rather than aborting on the first one, and a missing
// PVC is reported rather than silently skipped. message is the text of the
// first problem found, in scan order — the same one a real fail-fast clone
// attempt would hit first.
func walkTemplateStorage(ctx context.Context, c client.Client, templateNS, templateName string, vms []*unstructured.Unstructured) (missing []string, message string, err error) {
	record := func(entry, msg string) {
		missing = append(missing, entry)
		if message == "" {
			message = msg
		}
	}

	for _, vm := range vms {
		vmShortName := vm.GetLabels()[LabelVMName]
		volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")

		for _, vol := range volumes {
			volMap, ok := vol.(map[string]any)
			if !ok {
				continue
			}
			volName, _, _ := unstructured.NestedString(volMap, "name")
			pvcName := pvcNameForVolume(volMap)
			if pvcName == "" {
				continue // not PVC-backed (containerDisk, cloudInitDisk, ...) — nothing to check
			}

			entry, msg, healthy, gerr := checkTemplatePVC(ctx, c, templateNS, templateName, pvcName, volName)
			if gerr != nil {
				return nil, "", gerr
			}
			if !healthy {
				record(entry, msg)
				continue // don't check for a snapshot of a PVC that doesn't exist
			}
			entry, msg, unclonable, gerr := checkBaseSnapshot(ctx, c, templateNS, templateName, pvcName)
			if gerr != nil {
				return nil, "", gerr
			}
			if unclonable {
				record(entry, msg)
			}
		}

		if !hasEFIVarsPVCMount(vm) {
			continue
		}
		buildID := vm.GetLabels()[LabelBuildID]
		if buildID == "" || vmShortName == "" {
			continue // can't derive the efivars PVC name — same silent-skip as BuildInitialVolumeStates
		}
		efiPVCName := fmt.Sprintf("%s-%s-efivars", buildID, vmShortName)
		entry, msg, healthy, gerr := checkTemplatePVC(ctx, c, templateNS, templateName, efiPVCName, EFIVarsVolumeName)
		if gerr != nil {
			return nil, "", gerr
		}
		if !healthy {
			record(entry, msg)
			continue
		}
		entry, msg, unclonable, gerr := checkBaseSnapshot(ctx, c, templateNS, templateName, efiPVCName)
		if gerr != nil {
			return nil, "", gerr
		}
		if unclonable {
			record(entry, msg)
		}
	}

	return missing, message, nil
}

// checkTemplatePVC reports whether pvcName exists, is Bound, and its PV
// resolves. healthy=false with err=nil is a diagnosis (entry/message set);
// err!=nil is an infra failure that must abort the whole walk.
func checkTemplatePVC(ctx context.Context, c client.Client, ns, templateName, pvcName, volName string) (entry, message string, healthy bool, err error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if gerr := c.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: ns}, pvc); gerr != nil {
		if errors.IsNotFound(gerr) {
			return "pvc/" + pvcName, fmt.Sprintf(
				"template build %s's volume %s backing PVC %s no longer exists in namespace %s — "+
					"a clone would fail to provision this volume and the module must be rebuilt",
				templateName, volName, pvcName, ns), false, nil
		}
		return "", "", false, fmt.Errorf("getting template PVC %s: %w", pvcName, gerr)
	}
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		return "pvc/" + pvcName, fmt.Sprintf(
			"template build %s's PVC %s in namespace %s is not Bound — "+
				"a clone would fail to provision volume %s and the module must be rebuilt",
			templateName, pvcName, ns, volName), false, nil
	}

	pv := &corev1.PersistentVolume{}
	if gerr := c.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); gerr != nil {
		if errors.IsNotFound(gerr) {
			return "pv/" + pvc.Spec.VolumeName, fmt.Sprintf(
				"template build %s's PVC %s is bound to PersistentVolume %s which no longer exists — "+
					"the module must be rebuilt", templateName, pvcName, pvc.Spec.VolumeName), false, nil
		}
		return "", "", false, fmt.Errorf("getting PV %s: %w", pvc.Spec.VolumeName, gerr)
	}
	return "", "", true, nil
}

// checkBaseSnapshot reports whether any base VolumeSnapshot found for pvcName
// is terminal (being deleted). Zero matches is healthy — no snapshot yet is
// the normal pre-first-clone state; EnsureBaseSnapshotReady creates one
// on-demand, which this function must never do.
func checkBaseSnapshot(ctx context.Context, c client.Client, ns, templateName, pvcName string) (entry, message string, unclonable bool, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(volumeSnapshotListGVK)
	if lerr := c.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{
		LabelBaseSnapshot: "true",
		LabelSourcePVC:    pvcName,
	}); lerr != nil {
		return "", "", false, fmt.Errorf("listing base snapshots for pvc %s: %w", pvcName, lerr)
	}
	for _, snap := range list.Items {
		if snap.GetDeletionTimestamp() != nil {
			return "volumesnapshot/" + snap.GetName(), fmt.Sprintf(
				"template build %s's base snapshot %s for PVC %s in namespace %s is being deleted",
				templateName, snap.GetName(), pvcName, ns), true, nil
		}
	}
	return "", "", false, nil
}
