package kubevirt

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var kubevirtGVR = schema.GroupVersionResource{
	Group:    "kubevirt.io",
	Version:  "v1",
	Resource: "kubevirts",
}

var storageClassGVR = schema.GroupVersionResource{
	Group:    "storage.k8s.io",
	Version:  "v1",
	Resource: "storageclasses",
}

const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// UnmanagedBindingName is the KubeVirt network binding plugin the operator
// registers and references on NICs attached to `unmanaged` subnets. It uses
// domainAttachmentType=managedTap, which wires the VM tap to the pod interface
// through a bridge (like the core bridge binding) but WITHOUT the in-pod DHCP
// server, so a guest gateway VM (e.g. pfSense) owns DHCP on the segment instead
// of virt-launcher handing out the kube-ovn IPAM address. See
// docs/unmanaged-network-research.md.
const UnmanagedBindingName = "l2bridge"

// RequiredFeatureGates are the KubeVirt feature gates the operator needs.
// VMPersistentState graduated to GA in KubeVirt v1.6.0 and no longer needs
// to be listed here, but it does need vmStateStorageClass set on the KubeVirt
// CR so the persistent vTPM every build/clone VM carries can provision its
// per-VM backend PVC (see EnsureFeatureGates). NetworkBindingPlugins is
// required for the managedTap binding registered for unmanaged subnets (not
// GA in KubeVirt v1.8.x).
var RequiredFeatureGates = []string{
	"DataVolumes",
	"Sidecar",
	"Snapshot",
	"NetworkBindingPlugins",
}

// EnsureFeatureGates finds the KubeVirt CR and ensures the required feature
// gates are enabled and the unmanaged-subnet network binding plugin is
// registered. Missing gates are appended; existing ones are left as-is. The
// binding is merge-set so any other registered bindings are preserved.
func EnsureFeatureGates(ctx context.Context, cfg *rest.Config) error {
	logger := log.FromContext(ctx).WithName("kubevirt-featuregates")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	// List all KubeVirt CRs cluster-wide.
	list, err := client.Resource(kubevirtGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing KubeVirt CRs: %w", err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("no KubeVirt CR found in cluster")
	}

	kv := &list.Items[0]
	logger.Info("Found KubeVirt CR", "name", kv.GetName(), "namespace", kv.GetNamespace())

	// Read current feature gates from spec.configuration.developerConfiguration.featureGates.
	currentGates, _, _ := unstructured.NestedStringSlice(kv.Object,
		"spec", "configuration", "developerConfiguration", "featureGates")

	existing := make(map[string]bool, len(currentGates))
	for _, g := range currentGates {
		existing[g] = true
	}

	var missing []string
	for _, required := range RequiredFeatureGates {
		if !existing[required] {
			missing = append(missing, required)
		}
	}

	// Check whether our managedTap network binding plugin is already registered.
	binding, _, _ := unstructured.NestedMap(kv.Object,
		"spec", "configuration", "network", "binding", UnmanagedBindingName)
	bindingPresent := binding != nil

	// Ensure a backend storage class is set for persistent VM state. Every
	// build/clone VM carries a persistent vTPM, which KubeVirt backs with a
	// per-VM `persistent-state-for-<vm>` PVC provisioned from this class. If
	// it's unset we adopt the cluster's default storage class so persistent
	// TPM works without operator-side cluster config; an explicit value set by
	// an admin is left untouched.
	currentStateSC, _, _ := unstructured.NestedString(kv.Object,
		"spec", "configuration", "vmStateStorageClass")
	stateSC := ""
	if currentStateSC == "" {
		stateSC, err = defaultStorageClass(ctx, client)
		if err != nil {
			logger.Info("No vmStateStorageClass set and no default storage class found; "+
				"persistent vTPM will fail to start until one is configured", "error", err.Error())
		}
	}
	stateSCNeeded := currentStateSC == "" && stateSC != ""

	if len(missing) == 0 && bindingPresent && !stateSCNeeded {
		logger.Info("KubeVirt feature gates, network binding, and VM state storage already configured",
			"gates", RequiredFeatureGates, "binding", UnmanagedBindingName, "vmStateStorageClass", currentStateSC)
		return nil
	}

	// Append missing gates (preserving existing) and merge-set the binding.
	newGates := append(currentGates, missing...)
	logger.Info("Configuring KubeVirt feature gates, network binding, and VM state storage",
		"missing", missing, "gates", newGates, "binding", UnmanagedBindingName,
		"vmStateStorageClass", stateSC, "stateStorageClassNeeded", stateSCNeeded)

	configuration := map[string]any{
		"developerConfiguration": map[string]any{
			"featureGates": toInterfaceSlice(newGates),
		},
		// MergePatch on a map adds this key without dropping any
		// other bindings KubeVirt may already have registered.
		"network": map[string]any{
			"binding": map[string]any{
				UnmanagedBindingName: map[string]any{
					"domainAttachmentType": "managedTap",
				},
			},
		},
	}
	if stateSCNeeded {
		configuration["vmStateStorageClass"] = stateSC
	}

	patch := &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"configuration": configuration,
			},
		},
	}

	patchData, err := patch.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshaling patch: %w", err)
	}

	_, err = client.Resource(kubevirtGVR).Namespace(kv.GetNamespace()).Patch(
		ctx, kv.GetName(), types.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patching KubeVirt CR: %w", err)
	}

	logger.Info("KubeVirt feature gates and network binding updated successfully")
	return nil
}

// defaultStorageClass returns the name of the storage class marked as the
// cluster default via the storageclass.kubernetes.io/is-default-class
// annotation, used to back persistent VM state (vTPM) when the KubeVirt CR
// doesn't pin one explicitly.
func defaultStorageClass(ctx context.Context, client dynamic.Interface) (string, error) {
	list, err := client.Resource(storageClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing storage classes: %w", err)
	}
	for i := range list.Items {
		sc := &list.Items[i]
		if sc.GetAnnotations()[defaultStorageClassAnnotation] == "true" {
			return sc.GetName(), nil
		}
	}
	return "", fmt.Errorf("no default storage class found")
}

func toInterfaceSlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
