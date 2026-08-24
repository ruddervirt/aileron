// SPDX-License-Identifier: GPL-3.0-only

package clone

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const snapshotClassCacheDuration = 5 * time.Minute

// defaultSnapshotClassAnnotation marks a VolumeSnapshotClass as the cluster
// default for its CSI driver, mirroring the storage-class convention aileron
// already honors in internal/kubevirt/featuregates.go.
const defaultSnapshotClassAnnotation = "snapshot.storage.k8s.io/is-default-class"

// annotationTrue is the value a boolean opt-in annotation carries when enabled.
const annotationTrue = "true"

var volumeSnapshotGVK = schema.GroupVersionKind{
	Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot",
}

// snapshotClassResult is the cached outcome of resolving a CSI driver to a
// VolumeSnapshotClass: the chosen class name plus a warning that is non-empty
// when the choice was ambiguous (several classes matched and none was marked).
type snapshotClassResult struct {
	name    string
	warning string
}

// SnapshotClassCache caches VolumeSnapshotClass lookups to reduce API calls.
type SnapshotClassCache struct {
	mu        sync.RWMutex
	classes   map[string]snapshotClassResult // CSI driver -> resolved class
	expiresAt time.Time
}

func (c *SnapshotClassCache) Get(csiDriver string) (snapshotClassResult, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if time.Now().After(c.expiresAt) {
		return snapshotClassResult{}, false
	}
	res, ok := c.classes[csiDriver]
	return res, ok
}

func (c *SnapshotClassCache) Set(csiDriver string, res snapshotClassResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.classes == nil {
		c.classes = make(map[string]snapshotClassResult)
	}
	c.classes[csiDriver] = res
	c.expiresAt = time.Now().Add(snapshotClassCacheDuration)
}

// SnapshotManager handles CSI snapshot operations for clone workflows.
type SnapshotManager struct {
	Client     client.Client
	ClassCache SnapshotClassCache
}

// BuildInitialVolumeStates scans template VMs and builds the initial volume state list.
func (s *SnapshotManager) BuildInitialVolumeStates(ctx context.Context, templateVMs []*unstructured.Unstructured, templateNamespace string) ([]v1alpha1.CloneVolumeStatus, error) {
	logger := log.FromContext(ctx)
	var states []v1alpha1.CloneVolumeStatus

	for _, vm := range templateVMs {
		vmName := vm.GetName()
		// Short VM name is recorded on the template VM as a label at build
		// time (internal/build/template.go). Clone-side helpers use this to
		// derive names without parsing the template VM's full name.
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
				continue
			}

			// Resolve PVC -> PV chain.
			pvc := &corev1.PersistentVolumeClaim{}
			if err := s.Client.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: templateNamespace}, pvc); err != nil {
				if errors.IsNotFound(err) {
					logger.Info("Template PVC not found, skipping volume", "vm", vmName, "volume", volName, "pvc", pvcName)
					continue
				}
				return nil, fmt.Errorf("getting template PVC %s: %w", pvcName, err)
			}

			pvName := pvc.Spec.VolumeName
			if pvName == "" {
				return nil, fmt.Errorf("template PVC %s is not bound", pvcName)
			}

			// Get PV to find CSI driver and storage class.
			pv := &corev1.PersistentVolume{}
			if err := s.Client.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
				return nil, fmt.Errorf("getting PV %s: %w", pvName, err)
			}

			csiDriver := ""
			if pv.Spec.CSI != nil {
				csiDriver = pv.Spec.CSI.Driver
			}

			storageClassName := ""
			if pvc.Spec.StorageClassName != nil {
				storageClassName = *pvc.Spec.StorageClassName
			}

			requestedStorage := ""
			if req, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				requestedStorage = req.String()
			}

			state := v1alpha1.CloneVolumeStatus{
				VolumeName:        volName,
				SourceVMName:      vmName,
				SourceVMShortName: vmShortName,
				SourcePVCName:     pvcName,
				SourcePVName:      pvName,
				CSIDriver:         csiDriver,
				StorageClassName:  storageClassName,
				RequestedStorage:  requestedStorage,
				Phase:             v1alpha1.CloneVolumePhasePending,
			}

			states = append(states, state)
		}

		// Detect the efivars-style PVC. The sidecar hook mounts it outside
		// the normal volume list, so it won't be found by the volume scan
		// above.
		if !hasEFIVarsPVCMount(vm) {
			continue
		}
		buildID := vm.GetLabels()["ruddervirt.io/build-id"]
		if buildID == "" || vmShortName == "" {
			continue
		}
		efiPVCName := fmt.Sprintf("%s-%s-efivars", buildID, vmShortName)
		efiPVC := &corev1.PersistentVolumeClaim{}
		if err := s.Client.Get(ctx, types.NamespacedName{Name: efiPVCName, Namespace: templateNamespace}, efiPVC); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("No EFI PVC found for template VM, skipping", "vm", vmName, "pvc", efiPVCName)
				continue
			}
			return nil, fmt.Errorf("checking EFI PVC %s: %w", efiPVCName, err)
		}
		efiPVName := efiPVC.Spec.VolumeName
		if efiPVName == "" {
			return nil, fmt.Errorf("EFI PVC %s is not bound", efiPVCName)
		}
		efiPV := &corev1.PersistentVolume{}
		if err := s.Client.Get(ctx, types.NamespacedName{Name: efiPVName}, efiPV); err != nil {
			return nil, fmt.Errorf("getting PV %s for EFI PVC: %w", efiPVName, err)
		}
		efiCSI := ""
		if efiPV.Spec.CSI != nil {
			efiCSI = efiPV.Spec.CSI.Driver
		}
		efiSC := ""
		if efiPVC.Spec.StorageClassName != nil {
			efiSC = *efiPVC.Spec.StorageClassName
		}
		efiStorage := ""
		if req, ok := efiPVC.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			efiStorage = req.String()
		}
		states = append(states, v1alpha1.CloneVolumeStatus{
			VolumeName:        EFIVarsVolumeName,
			SourceVMName:      vmName,
			SourceVMShortName: vmShortName,
			SourcePVCName:     efiPVCName,
			SourcePVName:      efiPVName,
			CSIDriver:         efiCSI,
			StorageClassName:  efiSC,
			RequestedStorage:  efiStorage,
			Phase:             v1alpha1.CloneVolumePhasePending,
		})
	}

	return states, nil
}

// pvcNameForVolume returns the PVC name backing a template VM volume,
// resolved from either dataVolume.name or persistentVolumeClaim.claimName.
// Returns "" when the volume isn't PVC-backed (e.g. containerDisk).
func pvcNameForVolume(volMap map[string]any) string {
	if dv, ok := volMap["dataVolume"]; ok {
		if dvMap, ok := dv.(map[string]any); ok {
			if name, _, _ := unstructured.NestedString(dvMap, "name"); name != "" {
				return name
			}
		}
	}
	if pvc, ok := volMap["persistentVolumeClaim"]; ok {
		if pvcMap, ok := pvc.(map[string]any); ok {
			name, _, _ := unstructured.NestedString(pvcMap, "claimName")
			return name
		}
	}
	return ""
}

// hasEFIVarsPVCMount reports whether the template VM's VMI hookSidecars
// annotation declares a PVC mount — the actual signal that an efivars-style
// PVC exists for this VM (set whenever the build used efiFirmware and/or
// floppy; see internal/build.BuildHookSidecarsAnnotation). Checking this
// instead of spec.domain.firmware.bootloader.efi fixes floppy-only builds
// (no efiFirmware) never getting their efivars PVC snapshotted for clones,
// which left those clones with a stale hookSidecars PVC reference pointing
// at the (inaccessible, build-namespace) source PVC.
func hasEFIVarsPVCMount(vm *unstructured.Unstructured) bool {
	annotations, _, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "metadata", "annotations")
	raw, ok := annotations["hooks.kubevirt.io/hookSidecars"]
	if !ok || raw == "" {
		return false
	}
	var hooks []map[string]any
	if err := json.Unmarshal([]byte(raw), &hooks); err != nil {
		return false
	}
	for _, h := range hooks {
		if _, ok := h["pvc"]; ok {
			return true
		}
	}
	return false
}

// snapshotRestoreState classifies whether a VolumeSnapshot can back a clone PVC.
// terminal is true when the snapshot is gone or being deleted — a dead source the
// CSI provisioner will never satisfy ("snapshot is currently being deleted"); such
// a snapshot must never be wired into a new PVC. ready mirrors status.readyToUse.
// The fetched snapshot is returned (nil when NotFound) so callers can read further
// status fields without a second Get.
func snapshotRestoreState(ctx context.Context, c client.Client, name, namespace string) (snap *unstructured.Unstructured, ready, terminal bool, err error) {
	snapshot := &unstructured.Unstructured{}
	snapshot.SetGroupVersionKind(volumeSnapshotGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, snapshot); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, true, nil
		}
		return nil, false, false, err
	}
	if snapshot.GetDeletionTimestamp() != nil {
		return snapshot, false, true, nil
	}
	ready, _, _ = unstructured.NestedBool(snapshot.Object, "status", "readyToUse")
	return snapshot, ready, false, nil
}

// EnsureBaseSnapshotReady ensures a VolumeSnapshot exists for the given volume state.
// Returns true when the snapshot is ready to use.
func (s *SnapshotManager) EnsureBaseSnapshotReady(ctx context.Context, cloneName string, state *v1alpha1.CloneVolumeStatus, templateNamespace string) (bool, error) {
	logger := log.FromContext(ctx)

	// First check for an existing reusable snapshot.
	if state.SnapshotName == "" {
		existing, err := s.findReusableSnapshot(ctx, state, templateNamespace)
		if err != nil {
			return false, err
		}
		if existing != "" {
			state.SnapshotName = existing
			logger.Info("Found reusable snapshot", "snapshot", existing, "volume", state.VolumeName)
		}
	}

	// Create a new snapshot if none exists.
	if state.SnapshotName == "" {
		snapshotClass, warning, err := s.resolveSnapshotClass(ctx, state.CSIDriver)
		if err != nil {
			return false, fmt.Errorf("resolving snapshot class for %s: %w", state.CSIDriver, err)
		}
		state.SnapshotClassName = snapshotClass
		if warning != "" {
			logger.Info(warning, "volume", state.VolumeName, "csiDriver", state.CSIDriver, "snapshotClass", snapshotClass)
			state.Message = warning
		}

		snapshotName := truncateName(fmt.Sprintf("%s-%s-snap", state.SourcePVCName, cloneName))

		// The base snapshot is shared by every clone of this template, so its
		// lifecycle must NOT be tied to the clone that happens to create it.
		// Inherit the build-id/build labels from the source PVC so the snapshot
		// is garbage-collected with its build (cleanupBuildResources) instead.
		// Labeling it with the creator's clone id makes that clone's teardown
		// delete the snapshot out from under every other clone, stranding their
		// PVCs in Pending ("snapshot is currently being deleted").
		srcPVC := &corev1.PersistentVolumeClaim{}
		if err := s.Client.Get(ctx, types.NamespacedName{Name: state.SourcePVCName, Namespace: templateNamespace}, srcPVC); err != nil {
			return false, fmt.Errorf("getting source PVC %s for snapshot labels: %w", state.SourcePVCName, err)
		}
		snapLabels := map[string]string{
			LabelBaseSnapshot:         "true",
			LabelSourcePVC:            state.SourcePVCName,
			"ruddervirt.io/source-pv": state.SourcePVName,
			"ruddervirt.io/source-vm": state.SourceVMName,
		}
		for _, key := range []string{"ruddervirt.io/build-id", "ruddervirt.io/build", "ruddervirt.io/build-namespace"} {
			if v := srcPVC.Labels[key]; v != "" {
				snapLabels[key] = v
			}
		}

		snapshot := &unstructured.Unstructured{}
		snapshot.SetGroupVersionKind(volumeSnapshotGVK)
		snapshot.SetName(snapshotName)
		snapshot.SetNamespace(templateNamespace)
		snapshot.SetLabels(snapLabels)

		if err := unstructured.SetNestedField(snapshot.Object, snapshotClass, "spec", "volumeSnapshotClassName"); err != nil {
			return false, err
		}
		if err := unstructured.SetNestedField(snapshot.Object, map[string]any{
			"persistentVolumeClaimName": state.SourcePVCName,
			"kind":                      "PersistentVolumeClaim",
		}, "spec", "source"); err != nil {
			return false, err
		}

		if err := s.Client.Create(ctx, snapshot); err != nil {
			if errors.IsAlreadyExists(err) {
				state.SnapshotName = snapshotName
			} else {
				return false, fmt.Errorf("creating snapshot %s: %w", snapshotName, err)
			}
		} else {
			state.SnapshotName = snapshotName
			logger.Info("Created base snapshot", "snapshot", snapshotName, "pvc", state.SourcePVCName)
		}
	}

	// Check if snapshot is ready. A snapshot that has vanished (e.g. deleted by an
	// unrelated teardown) or carries a deletion timestamp can't be used as a PVC
	// data source — the CSI provisioner rejects it ("snapshot is currently being
	// deleted"). Forget it and reselect or recreate on the next pass.
	snapshot, readyToUse, terminal, err := snapshotRestoreState(ctx, s.Client, state.SnapshotName, templateNamespace)
	if err != nil {
		return false, fmt.Errorf("getting snapshot %s: %w", state.SnapshotName, err)
	}
	if terminal {
		state.SnapshotName = ""
		return false, nil
	}
	if !readyToUse {
		return false, nil
	}

	// Extract snapshot content name.
	contentName, _, _ := unstructured.NestedString(snapshot.Object, "status", "boundVolumeSnapshotContentName")
	if contentName != "" {
		state.SnapshotContentName = contentName
	}

	// Use snapshot restoreSize if it's larger than the requested storage.
	// CDI may round up volumes, so the snapshot size can exceed the original PVC request.
	restoreSizeStr, found, _ := unstructured.NestedString(snapshot.Object, "status", "restoreSize")
	if found && restoreSizeStr != "" {
		restoreSize, err := resource.ParseQuantity(restoreSizeStr)
		if err == nil && state.RequestedStorage != "" {
			requested, reqErr := resource.ParseQuantity(state.RequestedStorage)
			if reqErr == nil && restoreSize.Cmp(requested) > 0 {
				state.RequestedStorage = restoreSize.String()
			}
		} else if err == nil && state.RequestedStorage == "" {
			state.RequestedStorage = restoreSize.String()
		}
	}

	state.Phase = v1alpha1.CloneVolumePhaseSnapshotSelected
	return true, nil
}

func (s *SnapshotManager) findReusableSnapshot(ctx context.Context, state *v1alpha1.CloneVolumeStatus, templateNamespace string) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotList",
	})

	if err := s.Client.List(ctx, list,
		client.InNamespace(templateNamespace),
		client.MatchingLabels{
			LabelBaseSnapshot:         "true",
			LabelSourcePVC:            state.SourcePVCName,
			"ruddervirt.io/source-pv": state.SourcePVName,
			"ruddervirt.io/source-vm": state.SourceVMName,
		},
	); err != nil {
		return "", fmt.Errorf("listing snapshots: %w", err)
	}

	for _, snap := range list.Items {
		// Skip snapshots being deleted — provisioning a PVC from a terminating
		// snapshot fails, and selecting one only pins its finalizer longer.
		if snap.GetDeletionTimestamp() != nil {
			continue
		}
		readyToUse, _, _ := unstructured.NestedBool(snap.Object, "status", "readyToUse")
		if readyToUse {
			return snap.GetName(), nil
		}
	}

	return "", nil
}

// snapClassInfo is the subset of a VolumeSnapshotClass that selectSnapshotClass
// needs to make a choice among classes that all share the target CSI driver.
type snapClassInfo struct {
	name          string
	aileronMarked bool // AnnotationSnapshotClass == "true"
	isDefault     bool // defaultSnapshotClassAnnotation == "true"
}

// selectSnapshotClass picks one class name among candidates that all share the
// target CSI driver, applying aileron's precedence:
//  1. exactly one candidate -> use it;
//  2. classes marked with AnnotationSnapshotClass (aileron opt-in);
//  3. else classes marked with defaultSnapshotClassAnnotation (k8s default);
//  4. else all candidates.
//
// Within the first non-empty tier the names are sorted and the first is
// returned. ambiguous is true when the winning tier still held more than one
// candidate, so the caller can surface a warning. An empty slice returns "".
func selectSnapshotClass(candidates []snapClassInfo) (name string, ambiguous bool) {
	if len(candidates) == 0 {
		return "", false
	}
	if len(candidates) == 1 {
		return candidates[0].name, false
	}

	tier := filterSnapClasses(candidates, func(c snapClassInfo) bool { return c.aileronMarked })
	if len(tier) == 0 {
		tier = filterSnapClasses(candidates, func(c snapClassInfo) bool { return c.isDefault })
	}
	if len(tier) == 0 {
		tier = candidates
	}

	names := make([]string, len(tier))
	for i, c := range tier {
		names[i] = c.name
	}
	sort.Strings(names)
	return names[0], len(tier) > 1
}

func filterSnapClasses(in []snapClassInfo, keep func(snapClassInfo) bool) []snapClassInfo {
	var out []snapClassInfo
	for _, c := range in {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// resolveSnapshotClass resolves a CSI driver to a VolumeSnapshotClass name. The
// returned warning is non-empty when the choice was ambiguous (several classes
// matched the driver and none was marked); the clone still proceeds with the
// deterministic pick.
func (s *SnapshotManager) resolveSnapshotClass(ctx context.Context, csiDriver string) (string, string, error) {
	if res, ok := s.ClassCache.Get(csiDriver); ok {
		return res.name, res.warning, nil
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClassList",
	})

	if err := s.Client.List(ctx, list); err != nil {
		return "", "", fmt.Errorf("listing VolumeSnapshotClasses: %w", err)
	}

	var candidates []snapClassInfo
	for _, item := range list.Items {
		driver, _, _ := unstructured.NestedString(item.Object, "driver")
		if driver != csiDriver {
			continue
		}
		anns := item.GetAnnotations()
		candidates = append(candidates, snapClassInfo{
			name:          item.GetName(),
			aileronMarked: anns[AnnotationSnapshotClass] == annotationTrue,
			isDefault:     anns[defaultSnapshotClassAnnotation] == annotationTrue,
		})
	}

	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no VolumeSnapshotClass found for CSI driver %s", csiDriver)
	}

	name, ambiguous := selectSnapshotClass(candidates)
	warning := ""
	if ambiguous {
		names := make([]string, len(candidates))
		for i, c := range candidates {
			names[i] = c.name
		}
		sort.Strings(names)
		warning = fmt.Sprintf("ambiguous VolumeSnapshotClass for CSI driver %s: candidates %v; using %s — mark one with %s=true to disambiguate",
			csiDriver, names, name, AnnotationSnapshotClass)
	}

	s.ClassCache.Set(csiDriver, snapshotClassResult{name: name, warning: warning})
	return name, warning, nil
}
