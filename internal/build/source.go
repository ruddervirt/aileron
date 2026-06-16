package build

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// SourceImporter handles the SourceImporting phase for a single VM:
// creating a CDI DataVolume to import its base disk image.
// URL and containerDisk sources are cached in the operator namespace
// and cloned into per-build DataVolumes to avoid redundant downloads.
type SourceImporter struct {
	Client     client.Client
	OperatorNS string // namespace for cached sources (e.g. "ruddervirt-system")
}

func (s *SourceImporter) HandleVM(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (v1alpha1.VMPhase, error) {
	logger := logf.FromContext(ctx)
	dvName := BuildNameForBuildVMDataVolume(BuildID(build), vmSpec.Name)
	dvNamespace := BuildNS(build)

	// Check if the per-build DV already exists.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dvGVK)
	err := s.Client.Get(ctx, types.NamespacedName{Name: dvName, Namespace: dvNamespace}, existing)
	if err == nil {
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		logger.Info("Source DataVolume already exists", "dv", dvName, "ns", dvNamespace, "phase", phase)
		switch phase {
		case PhaseSucceeded:
			return v1alpha1.VMPhaseBooting, nil
		case PhaseFailed:
			return v1alpha1.VMPhaseFailed, fmt.Errorf("source DataVolume import failed for VM %s", vmSpec.Name)
		default:
			return v1alpha1.VMPhaseSourceImporting, nil
		}
	}
	if !errors.IsNotFound(err) {
		logger.Error(err, "Unexpected error checking DataVolume", "dv", dvName, "ns", dvNamespace)
		return v1alpha1.VMPhaseSourceImporting, fmt.Errorf("checking DataVolume: %w", err)
	}

	// Resolve the source (buildRef → PVC, etc.).
	resolved, err := s.resolveSource(ctx, build, vmSpec)
	if err != nil {
		return v1alpha1.VMPhaseFailed, err
	}

	// For cacheable sources (URL, containerDisk), use a two-phase approach:
	// 1. Ensure a cache DV exists in the operator namespace
	// 2. Create the per-build DV as a CDI clone from the cache
	cacheKey := SourceCacheKey(resolved)
	if cacheKey != "" {
		cacheNS := s.OperatorNS
		if cacheNS == "" {
			cacheNS = dvNamespace
		}
		cacheDVName := SourceCacheDVName(cacheKey)

		cacheReady, err := s.ensureCacheDV(ctx, vmSpec, resolved, cacheDVName, cacheNS)
		if err != nil {
			return v1alpha1.VMPhaseFailed, err
		}
		if !cacheReady {
			logger.Info("Waiting for source cache download", "cache", cacheDVName, "ns", cacheNS)
			return v1alpha1.VMPhaseSourceImporting, nil
		}

		// Cache is ready — create per-build DV as a clone from cache.
		cacheSize := s.lookupPVCSize(ctx, cacheDVName, cacheNS)
		resolved = &resolvedSource{pvcName: cacheDVName, pvcNamespace: cacheNS, sourceSize: cacheSize}
		logger.Info("Using cached source", "cache", cacheDVName, "ns", cacheNS)
	}

	dv, err := s.buildDataVolume(build, vmSpec, resolved, dvName, dvNamespace)
	if err != nil {
		return v1alpha1.VMPhaseFailed, fmt.Errorf("building DataVolume spec: %w", err)
	}

	if err := s.Client.Create(ctx, dv); err != nil {
		if errors.IsAlreadyExists(err) || errors.IsConflict(err) {
			logger.Info("DataVolume already exists (race), will requeue", "dv", dvName)
			return v1alpha1.VMPhaseSourceImporting, nil
		}
		return v1alpha1.VMPhaseFailed, fmt.Errorf("creating DataVolume: %w", err)
	}
	logger.Info("Created source DataVolume", "dv", dvName, "ns", dvNamespace)

	// Create blank DataVolumes for additional disks (index > 0).
	for i, d := range DefaultDisks(vmSpec) {
		if i == 0 {
			continue
		}
		extraName := DiskDVName(BuildID(build), vmSpec.Name, i, d.Name)
		extraDV := &unstructured.Unstructured{}
		extraDV.SetGroupVersionKind(dvGVK)
		extraDV.SetName(extraName)
		extraDV.SetNamespace(dvNamespace)
		extraDV.SetLabels(map[string]string{
			LabelBuildID:        BuildID(build),
			LabelBuild:          build.Name,
			LabelBuildNamespace: build.Namespace,
			LabelVM:             vmSpec.Name,
		})
		extraSpec := map[string]any{
			"source": map[string]any{"blank": map[string]any{}},
			"storage": map[string]any{
				"resources": map[string]any{
					"requests": map[string]any{
						"storage": d.Size.String(),
					},
				},
				"volumeMode": "Filesystem",
			},
		}
		if d.StorageClass != nil {
			extraSpec["storage"].(map[string]any)["storageClassName"] = *d.StorageClass
		}
		if err := unstructured.SetNestedField(extraDV.Object, extraSpec, "spec"); err != nil {
			return v1alpha1.VMPhaseFailed, fmt.Errorf("building extra disk DV: %w", err)
		}
		if err := s.Client.Create(ctx, extraDV); err != nil && !errors.IsAlreadyExists(err) {
			return v1alpha1.VMPhaseFailed, fmt.Errorf("creating extra disk DV %s: %w", extraName, err)
		}
		logger.Info("Created extra disk DataVolume", "dv", extraName, "disk", d.Name)
	}

	return v1alpha1.VMPhaseSourceImporting, nil
}

// ensureCacheDV ensures a cached source DataVolume exists in the operator namespace.
// Returns true when the cache DV is ready (Succeeded).
func (s *SourceImporter) ensureCacheDV(ctx context.Context, vmSpec *v1alpha1.BuildVM, src *resolvedSource, name, namespace string) (bool, error) {
	logger := logf.FromContext(ctx)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dvGVK)
	err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)

	if err == nil {
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		switch phase {
		case PhaseSucceeded:
			// Touch last-used annotation to refresh cache TTL.
			annotations := existing.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations["ruddervirt.io/source-last-used"] = time.Now().UTC().Format(time.RFC3339)
			existing.SetAnnotations(annotations)
			_ = s.Client.Update(ctx, existing) // best-effort
			return true, nil
		case PhaseFailed:
			return false, fmt.Errorf("cached source import failed for %s", name)
		default:
			return false, nil // still importing
		}
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("checking cached source DV %s: %w", name, err)
	}

	// Create the cache DV.
	bootDisk := BootDisk(vmSpec)
	diskSize := bootDisk.Size
	if diskSize.IsZero() {
		diskSize = resource.MustParse("20Gi")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dvGVK)
	dv.SetName(name)
	dv.SetNamespace(namespace)
	dv.SetLabels(map[string]string{
		"ruddervirt.io/source-cache": "true",
	})
	dv.SetAnnotations(map[string]string{
		"ruddervirt.io/source-last-used": now,
	})

	spec := map[string]any{
		"storage": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": diskSize.String(),
				},
			},
			"volumeMode": "Filesystem",
		},
	}

	switch {
	case src.url != "":
		spec["source"] = map[string]any{
			"http": map[string]any{"url": src.url},
		}
	case src.containerDisk != "":
		spec["source"] = map[string]any{
			"registry": map[string]any{"url": "docker://" + src.containerDisk},
		}
	}

	if err := unstructured.SetNestedField(dv.Object, spec, "spec"); err != nil {
		return false, err
	}
	if err := s.Client.Create(ctx, dv); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil // concurrent build created it
		}
		return false, fmt.Errorf("creating cached source DV: %w", err)
	}
	logger.Info("Created source cache DataVolume", "dv", name, "ns", namespace)
	return false, nil
}

// resolvedSource holds the resolved source for DataVolume creation.
// A buildRef is resolved into the equivalent of a sourcePVC.
type resolvedSource struct {
	url           string
	pvcName       string
	pvcNamespace  string
	containerDisk string
	blank         bool
	// sourceSize is the actual storage size of the source PVC (populated for
	// PVC-based clones). Used by buildDataVolume to ensure the target DV is
	// at least as large as the source + headroom.
	sourceSize *resource.Quantity
}

// pvcCloneHeadroom is added to the source PVC size when the spec's requested
// disk is smaller than the source. This ensures CDI never rejects a clone
// because the target is smaller than the source.
const pvcCloneHeadroom = "3Gi"

// resolveSource resolves a BuildSource, looking up buildRef if specified.
func (s *SourceImporter) resolveSource(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (*resolvedSource, error) {
	src := vmSpec.Source

	switch {
	case src.URL != "":
		return &resolvedSource{url: src.URL}, nil

	case src.SourcePVC != nil:
		sourceSize := s.lookupPVCSize(ctx, src.SourcePVC.Name, src.SourcePVC.Namespace)
		return &resolvedSource{pvcName: src.SourcePVC.Name, pvcNamespace: src.SourcePVC.Namespace, sourceSize: sourceSize}, nil

	case src.ContainerDisk != "":
		return &resolvedSource{containerDisk: src.ContainerDisk}, nil

	case src.BuildRef != nil:
		return s.resolveBuildRef(ctx, build, vmSpec, src.BuildRef)

	case src.Blank:
		return &resolvedSource{blank: true}, nil

	default:
		return nil, fmt.Errorf("VM %s: no source specified (set url, sourcePvc, containerDisk, blank, or buildRef)", vmSpec.Name)
	}
}

// resolveBuildRef looks up a previous VirtualMachineBuild, verifies it succeeded,
// and returns the output DataVolume as a PVC source.
func (s *SourceImporter) resolveBuildRef(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, ref *v1alpha1.BuildReference) (*resolvedSource, error) {
	refNamespace := ref.Namespace
	if refNamespace == "" {
		refNamespace = build.Namespace
	}

	// Fetch the referenced build.
	upstreamBuild := &v1alpha1.VirtualMachineBuild{}
	err := s.Client.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: refNamespace}, upstreamBuild)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("VM %s: referenced build %s/%s not found", vmSpec.Name, refNamespace, ref.Name)
		}
		return nil, fmt.Errorf("VM %s: fetching referenced build %s/%s: %w", vmSpec.Name, refNamespace, ref.Name, err)
	}

	// The upstream build must be Succeeded.
	if upstreamBuild.Status.Phase != v1alpha1.BuildPhaseSucceeded {
		return nil, fmt.Errorf("VM %s: referenced build %s/%s is in phase %s, must be Succeeded",
			vmSpec.Name, refNamespace, ref.Name, upstreamBuild.Status.Phase)
	}

	// Find the output DataVolume for the specified VM (or the only VM if vmName is empty).
	var targetVMStatus *v1alpha1.VMBuildStatus
	if ref.VMName != "" {
		for i := range upstreamBuild.Status.VMStatuses {
			if upstreamBuild.Status.VMStatuses[i].Name == ref.VMName {
				targetVMStatus = &upstreamBuild.Status.VMStatuses[i]
				break
			}
		}
		if targetVMStatus == nil {
			return nil, fmt.Errorf("VM %s: referenced build %s/%s has no VM named %q",
				vmSpec.Name, refNamespace, ref.Name, ref.VMName)
		}
	} else {
		// No vmName specified — the upstream build must have exactly one VM.
		if len(upstreamBuild.Status.VMStatuses) != 1 {
			return nil, fmt.Errorf("VM %s: referenced build %s/%s has %d VMs, must specify vmName",
				vmSpec.Name, refNamespace, ref.Name, len(upstreamBuild.Status.VMStatuses))
		}
		targetVMStatus = &upstreamBuild.Status.VMStatuses[0]
	}

	if targetVMStatus.OutputDataVolume == "" {
		return nil, fmt.Errorf("VM %s: referenced build %s/%s VM %s has no output DataVolume",
			vmSpec.Name, refNamespace, ref.Name, targetVMStatus.Name)
	}

	// Parse "namespace/name" from outputDataVolume.
	dvNamespace, dvName, err := parseNamespacedName(targetVMStatus.OutputDataVolume)
	if err != nil {
		return nil, fmt.Errorf("VM %s: parsing output DataVolume reference: %w", vmSpec.Name, err)
	}

	// Look up the source PVC size so buildDataVolume can ensure the target
	// is at least as large (CDI rejects clones where target < source).
	sourceSize := s.lookupPVCSize(ctx, dvName, dvNamespace)

	return &resolvedSource{pvcName: dvName, pvcNamespace: dvNamespace, sourceSize: sourceSize}, nil
}

func parseNamespacedName(s string) (namespace, name string, err error) {
	for i := range s {
		if s[i] == '/' {
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("expected namespace/name format, got %q", s)
}

// lookupPVCSize returns the storage request of a PVC, or nil if it can't be read.
// Best-effort: a nil return just means we won't auto-bump the target DV size.
func (s *SourceImporter) lookupPVCSize(ctx context.Context, name, namespace string) *resource.Quantity {
	pvc := &unstructured.Unstructured{}
	pvc.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"})
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pvc); err != nil {
		return nil
	}
	sizeStr, _, _ := unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	if sizeStr == "" {
		return nil
	}
	q, err := resource.ParseQuantity(sizeStr)
	if err != nil {
		return nil
	}
	return &q
}

func (s *SourceImporter) buildDataVolume(build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, src *resolvedSource, name, namespace string) (*unstructured.Unstructured, error) {
	bootDisk := BootDisk(vmSpec)
	diskSize := bootDisk.Size
	if diskSize.IsZero() {
		diskSize = resource.MustParse("20Gi")
	}

	// When cloning from a PVC, ensure the target is at least as large as
	// the source + 3Gi headroom. CDI rejects clones where the target is
	// smaller than the source, and Windows images grow during updates.
	if src.sourceSize != nil {
		headroom := resource.MustParse(pvcCloneHeadroom)
		minSize := src.sourceSize.DeepCopy()
		minSize.Add(headroom)
		if diskSize.Cmp(minSize) < 0 {
			logger := logf.Log.WithName("source-importer")
			logger.Info("Increasing disk size to cover source PVC + headroom",
				"vm", vmSpec.Name, "specSize", diskSize.String(),
				"sourceSize", src.sourceSize.String(), "adjustedSize", minSize.String())
			diskSize = minSize
		}
	}

	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume",
	})
	dv.SetName(name)
	dv.SetNamespace(namespace)
	dv.SetLabels(map[string]string{
		LabelBuildID:        BuildID(build),
		LabelBuild:          build.Name,
		LabelBuildNamespace: build.Namespace,
		LabelVM:             vmSpec.Name,
	})

	spec := map[string]any{
		"storage": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": diskSize.String(),
				},
			},
			"volumeMode": "Filesystem",
		},
	}

	if bootDisk.StorageClass != nil {
		spec["storage"].(map[string]any)["storageClassName"] = *bootDisk.StorageClass
	}

	switch {
	case src.url != "":
		spec["source"] = map[string]any{
			"http": map[string]any{"url": src.url},
		}
	case src.pvcName != "":
		spec["source"] = map[string]any{
			"pvc": map[string]any{
				"name":      src.pvcName,
				"namespace": src.pvcNamespace,
			},
		}
	case src.containerDisk != "":
		spec["source"] = map[string]any{
			"registry": map[string]any{"url": "docker://" + src.containerDisk},
		}
	case src.blank:
		spec["source"] = map[string]any{
			"blank": map[string]any{},
		}
	default:
		return nil, fmt.Errorf("VM %s: resolved source has no type", vmSpec.Name)
	}

	if err := unstructured.SetNestedField(dv.Object, spec, "spec"); err != nil {
		return nil, err
	}

	return dv, nil
}
