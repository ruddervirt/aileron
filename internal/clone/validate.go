package clone

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var vmGVK = schema.GroupVersionKind{
	Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
}

// findBuild returns the VirtualMachineBuild whose name matches templateName,
// searched across all namespaces. Returns (nil, nil) when no such build exists.
func findBuild(ctx context.Context, c client.Client, templateName string) (*v1alpha1.VirtualMachineBuild, error) {
	buildList := &v1alpha1.VirtualMachineBuildList{}
	if err := c.List(ctx, buildList); err != nil {
		return nil, fmt.Errorf("listing builds: %w", err)
	}
	for i := range buildList.Items {
		if buildList.Items[i].Name == templateName {
			return &buildList.Items[i], nil
		}
	}
	return nil, nil
}

// TemplateNamespace finds the namespace containing template VMs for a build.
// Looks up the VirtualMachineBuild CR's status.templateNamespace.
func TemplateNamespace(ctx context.Context, c client.Client, templateName string) (string, error) {
	b, err := findBuild(ctx, c, templateName)
	if err != nil {
		return "", err
	}
	if b == nil {
		return "", fmt.Errorf("build %s not found", templateName)
	}
	if b.Status.TemplateNamespace != "" {
		return b.Status.TemplateNamespace, nil
	}
	if b.Status.BuildNamespace != "" {
		return b.Status.BuildNamespace, nil
	}
	return "", fmt.Errorf("build %s has no template namespace set", templateName)
}

// ListTemplateVMs lists template VirtualMachines for a specific build.
func ListTemplateVMs(ctx context.Context, c client.Client, templateNamespace, templateName string) ([]*unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList",
	})

	if err := c.List(ctx, list,
		client.InNamespace(templateNamespace),
		client.MatchingLabels{
			"ruddervirt.io/build":     templateName,
			"ruddervirt.io/component": "template",
		},
	); err != nil {
		return nil, fmt.Errorf("listing template VMs for build %s: %w", templateName, err)
	}

	result := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		result[i] = &list.Items[i]
	}
	return result, nil
}

// templateOutputVolumesExist reports whether any output DataVolumes for the
// build still exist in the template namespace. When the template VMs are gone
// but their output volumes remain, the template was deleted *after* a
// successful build and can be rebuilt from the surviving disks — a materially
// different (and recoverable) situation from a build that never produced any
// template output at all. Best-effort: a list error is treated as "absent" so
// the caller falls back to the more generic diagnostic.
func templateOutputVolumesExist(ctx context.Context, c client.Client, templateNamespace, templateName string) bool {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolumeList",
	})
	if err := c.List(ctx, list,
		client.InNamespace(templateNamespace),
		client.MatchingLabels{"ruddervirt.io/build": templateName},
	); err != nil {
		return false
	}
	return len(list.Items) > 0
}

// ValidateTemplate checks that the template backing templateName is present and
// usable, returning its namespace and template VMs. Every failure path returns
// an actionable error: the message a clone fails with (and that surfaces in
// rudder-ui / Grafana) names the exact remediation rather than a bare
// "no VirtualMachines found in template namespace <ns>", which cannot be told
// apart from an in-flight or failed build.
func ValidateTemplate(ctx context.Context, c client.Client, templateName string) (string, []*unstructured.Unstructured, error) {
	b, err := findBuild(ctx, c, templateName)
	if err != nil {
		return "", nil, fmt.Errorf("validating template %s: %w", templateName, err)
	}
	if b == nil {
		return "", nil, fmt.Errorf("template build %s not found", templateName)
	}

	// A build only labels its VMs `component: template` in the terminal
	// TemplateProvisioning step, so an unfinished build never has template VMs.
	// Reporting that as "no VMs found" (the old behaviour, via the
	// BuildNamespace fallback) is misleading — the template simply is not ready.
	if b.Status.Phase != v1alpha1.BuildPhaseSucceeded {
		return "", nil, fmt.Errorf(
			"template build %s is not ready (build phase %q); clone cannot proceed until the build succeeds",
			templateName, b.Status.Phase)
	}

	templateNS := b.Status.TemplateNamespace
	if templateNS == "" {
		templateNS = b.Status.BuildNamespace
	}
	if templateNS == "" {
		return "", nil, fmt.Errorf("template build %s succeeded but records no template namespace", templateName)
	}

	vms, err := ListTemplateVMs(ctx, c, templateNS, templateName)
	if err != nil {
		return "", nil, fmt.Errorf("validating template %s: %w", templateName, err)
	}

	if len(vms) == 0 {
		if templateOutputVolumesExist(ctx, c, templateNS, templateName) {
			return "", nil, fmt.Errorf(
				"template build %s succeeded but its template VirtualMachines are missing from namespace %s "+
					"while their output volumes remain — the template VMs were deleted after building and the module must be rebuilt",
				templateName, templateNS)
		}
		return "", nil, fmt.Errorf(
			"template build %s succeeded but neither template VirtualMachines nor output volumes exist in namespace %s "+
				"— the build output was fully removed and the module must be rebuilt",
			templateName, templateNS)
	}

	return templateNS, vms, nil
}
