package build

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var dvGVK = schema.GroupVersionKind{
	Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume",
}

// ISOImporter handles importing ISO images as cached DataVolumes in the operator
// namespace, then cloning them into per-build namespaces.
type ISOImporter struct {
	Client     client.Client
	OperatorNS string // namespace for cached ISOs (e.g. "ruddervirt-system")
}

// HandleISOs ensures all ISO images for a VM are available in the build namespace.
// Uses a two-phase approach:
//  1. Cache phase: download the ISO into the operator namespace (shared across builds)
//  2. Clone phase: CDI cross-namespace clone from operator NS into the build namespace
//
// Returns true when every ISO clone in the build namespace is ready.
func (iso *ISOImporter) HandleISOs(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) (bool, error) {
	logger := log.FromContext(ctx)
	cacheNS := iso.OperatorNS
	if cacheNS == "" {
		cacheNS = BuildNS(build)
	}
	allReady := true

	for i := range vmSpec.ISOs {
		isoSpec := &vmSpec.ISOs[i]
		cacheKey := ISOCacheKey(isoSpec)
		cacheName := ISOCacheDVName(cacheKey)

		// Phase 1: Ensure the ISO is cached in the operator namespace.
		cacheReady, err := iso.ensureCacheDV(ctx, cacheNS, cacheName, isoSpec, cacheKey)
		if err != nil {
			return false, fmt.Errorf("ensuring ISO cache for %s: %w", isoSpec.URL, err)
		}
		if !cacheReady {
			logger.Info("Waiting for ISO cache download", "iso", cacheName, "namespace", cacheNS)
			allReady = false
			continue
		}

		// Phase 2: Ensure a per-build clone DV exists in the build namespace.
		// The launcher mounts the clone, leaving the cached ISO unattached so
		// other concurrent builds can clone it too. Required for any storage
		// class without RWX (most block CSI drivers): RWO can only be
		// attached to one launcher at a time.
		cloneName := ISOCloneDVName(BuildID(build), vmSpec.Name, i)
		cloneReady, err := iso.ensureCloneDV(ctx, build, vmSpec, BuildNS(build), cacheNS, cloneName, cacheName)
		if err != nil {
			return false, fmt.Errorf("ensuring ISO clone for %s: %w", cacheName, err)
		}
		if !cloneReady {
			logger.Info("Waiting for ISO clone", "clone", cloneName, "source", cacheName)
			allReady = false
		}
	}

	return allReady, nil
}

// ensureCloneDV creates a per-build clone of a cached ISO and waits for it
// to reach Succeeded. The clone DV carries the build-id and iso-clone labels
// so existing cleanup paths (cleanupBuildResources on failure/deletion,
// cleanupEphemeralResources at template provisioning) sweep it up without
// any new wiring. CDI handles the cross-namespace clone transparently;
// when source and target are in the same namespace it's a fast metadata
// clone on most CSI drivers.
func (iso *ISOImporter) ensureCloneDV(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, namespace, sourceNS, name, sourceName string) (bool, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dvGVK)
	err := iso.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)
	if err == nil {
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		switch phase {
		case PhaseSucceeded:
			return true, nil
		case PhaseFailed:
			return false, fmt.Errorf("ISO clone DV %s failed", name)
		default:
			return false, nil
		}
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("checking ISO clone DV %s: %w", name, err)
	}

	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dvGVK)
	dv.SetName(name)
	dv.SetNamespace(namespace)
	dv.SetLabels(map[string]string{
		LabelBuildID:              BuildID(build),
		LabelBuild:                build.Name,
		LabelBuildNamespace:       build.Namespace,
		LabelVM:                   vmSpec.Name,
		"ruddervirt.io/iso-clone": "true",
	})
	dv.Object["spec"] = map[string]any{
		"storage": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": "10Gi",
				},
			},
			"volumeMode":  "Filesystem",
			"accessModes": []any{"ReadWriteOnce"},
		},
		"source": map[string]any{
			"pvc": map[string]any{
				"name":      sourceName,
				"namespace": sourceNS,
			},
		},
	}
	if err := iso.Client.Create(ctx, dv); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating ISO clone DV: %w", err)
	}
	return false, nil
}

// ensureCacheDV ensures the ISO DataVolume exists and is ready in the cache namespace.
func (iso *ISOImporter) ensureCacheDV(ctx context.Context, namespace, name string, isoSpec *v1alpha1.ISOSource, cacheKey string) (bool, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dvGVK)
	err := iso.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, existing)

	if err == nil {
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		switch phase {
		case PhaseSucceeded:
			// Touch last-used annotation to refresh cache TTL.
			annotations := existing.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations["ruddervirt.io/iso-last-used"] = time.Now().UTC().Format(time.RFC3339)
			existing.SetAnnotations(annotations)
			_ = iso.Client.Update(ctx, existing) // best-effort TTL refresh
			return true, nil
		case PhaseFailed:
			return false, fmt.Errorf("ISO import failed for %s", isoSpec.URL)
		default:
			return false, nil // still importing
		}
	}

	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("checking ISO DataVolume %s: %w", name, err)
	}

	// Create new ISO DataVolume (download from URL).
	dv := iso.buildISODataVolume(namespace, name, isoSpec, cacheKey)
	if err := iso.Client.Create(ctx, dv); err != nil {
		if errors.IsAlreadyExists(err) {
			return false, nil // concurrent build created it, wait
		}
		return false, fmt.Errorf("creating ISO DataVolume: %w", err)
	}
	return false, nil // just created, wait for download
}

func (iso *ISOImporter) buildISODataVolume(namespace, name string, isoSpec *v1alpha1.ISOSource, cacheKey string) *unstructured.Unstructured {
	now := time.Now().UTC().Format(time.RFC3339)

	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dvGVK)
	dv.SetName(name)
	dv.SetNamespace(namespace)
	dv.SetLabels(map[string]string{
		"ruddervirt.io/iso-cache":     "true",
		"ruddervirt.io/iso-cache-key": cacheKey[:16],
	})
	dv.SetAnnotations(map[string]string{
		"ruddervirt.io/iso-url":       isoSpec.URL,
		"ruddervirt.io/iso-last-used": now,
	})
	// No owner references — ISOs are shared cached resources.

	spec := map[string]any{
		"storage": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": "10Gi",
				},
			},
			"volumeMode":  "Filesystem",
			"accessModes": []any{"ReadWriteOnce"},
		},
		"source": map[string]any{
			"http": map[string]any{"url": isoSpec.URL},
		},
	}

	dv.Object["spec"] = spec
	return dv
}

// CleanupExpiredISOs deletes cached ISO DataVolumes that haven't been used within the TTL.
func (iso *ISOImporter) CleanupExpiredISOs(ctx context.Context, namespace string, ttl time.Duration) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList",
	})

	if err := iso.Client.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{"ruddervirt.io/iso-cache": "true"},
	); err != nil {
		return fmt.Errorf("listing cached ISOs: %w", err)
	}

	for i := range list.Items {
		dv := &list.Items[i]
		// Skip clone DVs — only clean up source cache DVs.
		if dv.GetLabels()["ruddervirt.io/iso-clone"] == "true" {
			continue
		}
		annotations := dv.GetAnnotations()
		lastUsedStr := annotations["ruddervirt.io/iso-last-used"]
		if lastUsedStr == "" {
			continue
		}
		lastUsed, err := time.Parse(time.RFC3339, lastUsedStr)
		if err != nil {
			continue
		}
		if time.Since(lastUsed) > ttl {
			// Only delete if import is complete (don't delete mid-import DVs).
			phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
			if phase == PhaseSucceeded || phase == PhaseFailed {
				if err := iso.Client.Delete(ctx, dv); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("deleting expired ISO %s: %w", dv.GetName(), err)
				}
			}
		}
	}

	return nil
}
