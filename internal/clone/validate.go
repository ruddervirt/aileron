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

// TemplateNamespace finds the namespace containing template VMs for a build.
// Looks up the VirtualMachineBuild CR's status.templateNamespace.
func TemplateNamespace(ctx context.Context, c client.Client, templateName string) (string, error) {
	// List builds matching the template name across all namespaces.
	buildList := &v1alpha1.VirtualMachineBuildList{}
	if err := c.List(ctx, buildList); err != nil {
		return "", fmt.Errorf("listing builds: %w", err)
	}
	for i := range buildList.Items {
		if buildList.Items[i].Name == templateName {
			b := &buildList.Items[i]
			if b.Status.TemplateNamespace != "" {
				return b.Status.TemplateNamespace, nil
			}
			if b.Status.BuildNamespace != "" {
				return b.Status.BuildNamespace, nil
			}
			return "", fmt.Errorf("build %s has no template namespace set", templateName)
		}
	}
	return "", fmt.Errorf("build %s not found", templateName)
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

// ValidateTemplate checks that the template namespace exists and contains VMs.
func ValidateTemplate(ctx context.Context, c client.Client, templateName string) (string, []*unstructured.Unstructured, error) {
	templateNS, err := TemplateNamespace(ctx, c, templateName)
	if err != nil {
		return "", nil, fmt.Errorf("validating template %s: %w", templateName, err)
	}

	vms, err := ListTemplateVMs(ctx, c, templateNS, templateName)
	if err != nil {
		return "", nil, fmt.Errorf("validating template %s: %w", templateName, err)
	}

	if len(vms) == 0 {
		return "", nil, fmt.Errorf("no VirtualMachines found in template namespace %s", templateNS)
	}

	return templateNS, vms, nil
}
