package kubevirt

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RequiredEmulatedMachinesAMD64 are the machine-type patterns the operator
// needs in KubeVirt's amd64 allowed list. q35*/pc-q35* are KubeVirt's upstream
// defaults — listed explicitly so we don't silently drop them when patching.
// pc-i440fx* is what aileron's build VMs use: RHEL's qemu-kvm gates isa-fdc
// off for q35, so the autounattend floppy path requires i440fx.
var RequiredEmulatedMachinesAMD64 = []string{
	"q35*",
	"pc-q35*",
	"pc-i440fx*",
}

// EnsureEmulatedMachines patches the KubeVirt CR's
// spec.configuration.architectureConfiguration.amd64.emulatedMachines so the
// admission webhook accepts our build VMs' machine type. Missing entries are
// appended; existing ones are left as-is.
func EnsureEmulatedMachines(ctx context.Context, cfg *rest.Config) error {
	logger := log.FromContext(ctx).WithName("kubevirt-emulatedmachines")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("creating dynamic client: %w", err)
	}

	list, err := client.Resource(kubevirtGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing KubeVirt CRs: %w", err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("no KubeVirt CR found in cluster")
	}

	kv := &list.Items[0]

	current, _, _ := unstructured.NestedStringSlice(kv.Object,
		"spec", "configuration", "architectureConfiguration", "amd64", "emulatedMachines")

	existing := make(map[string]bool, len(current))
	for _, m := range current {
		existing[m] = true
	}

	var missing []string
	for _, required := range RequiredEmulatedMachinesAMD64 {
		if !existing[required] {
			missing = append(missing, required)
		}
	}

	if len(missing) == 0 {
		logger.Info("All required amd64 emulated machine patterns already allowed", "patterns", RequiredEmulatedMachinesAMD64)
		return nil
	}

	// When the field is unset upstream, KubeVirt falls back to its built-in
	// defaults (q35*, pc-q35*). Setting this explicitly takes over from the
	// defaults entirely, so the required list above must include those
	// defaults, not just the additions we're after.
	newList := append(current, missing...)
	logger.Info("Adding amd64 emulated machine patterns", "missing", missing, "all", newList)

	patch := &unstructured.Unstructured{
		Object: map[string]any{
			"spec": map[string]any{
				"configuration": map[string]any{
					"architectureConfiguration": map[string]any{
						"amd64": map[string]any{
							"emulatedMachines": toInterfaceSlice(newList),
						},
					},
				},
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

	logger.Info("KubeVirt amd64 emulated machines updated successfully")
	return nil
}
