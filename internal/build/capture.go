package build

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DiskCapturer handles the DiskCaptured phase for a single VM:
// cloning its build disk into the output DataVolume.
type DiskCapturer struct {
	Client client.Client
}

func (d *DiskCapturer) HandleVM(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (v1alpha1.VMPhase, error) {
	// Output DVs are created in the build namespace (which becomes the template namespace).
	outputNS := BuildNS(build)
	outputDVName := BuildNameForOutputDV(BuildID(build), vmSpec.Name)

	// Check if output DV exists.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume",
	})
	err := d.Client.Get(ctx, types.NamespacedName{Name: outputDVName, Namespace: outputNS}, existing)
	if err == nil {
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		switch phase {
		case PhaseSucceeded:
			vmStatus.OutputDataVolume = fmt.Sprintf("%s/%s", outputNS, outputDVName)
			return v1alpha1.VMPhaseSucceeded, nil
		case PhaseFailed:
			return v1alpha1.VMPhaseFailed, fmt.Errorf("output DataVolume clone failed for VM %s", vmSpec.Name)
		default:
			return v1alpha1.VMPhaseDiskCaptured, nil
		}
	}
	if !errors.IsNotFound(err) {
		return v1alpha1.VMPhaseDiskCaptured, fmt.Errorf("checking output DataVolume: %w", err)
	}

	// Clone the source DV into the output.
	srcDVName := BuildNameForBuildVMDataVolume(BuildID(build), vmSpec.Name)
	srcNS := build.Status.BuildNamespace
	if srcNS == "" {
		srcNS = build.Namespace
	}

	// Use the actual source PVC size — the on-disk PVC may be larger than
	// the spec's requested size due to filesystem overhead, CDI rounding,
	// or Rook Ceph minimum allocation. CDI rejects clones where target < source.
	logger := logf.FromContext(ctx)
	bootDisk := BootDisk(vmSpec)
	diskSize := bootDisk.Size
	srcPVC := &corev1.PersistentVolumeClaim{}
	if err := d.Client.Get(ctx, types.NamespacedName{Name: srcDVName, Namespace: srcNS}, srcPVC); err == nil {
		if actual := srcPVC.Status.Capacity.Storage(); actual != nil && actual.Cmp(diskSize) > 0 {
			logger.Info("Output DV: using source PVC actual size instead of spec size",
				"vm", vmSpec.Name, "specSize", diskSize.String(), "actualSize", actual.String())
			diskSize = *actual
		}
	}

	spec := map[string]any{
		"source": map[string]any{
			"pvc": map[string]any{
				"name":      srcDVName,
				"namespace": srcNS,
			},
		},
		"storage": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{
					"storage": diskSize.String(),
				},
			},
			"volumeMode": "Filesystem",
		},
	}

	dv := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata": map[string]any{
				"name":      outputDVName,
				"namespace": outputNS,
				"labels": map[string]any{
					LabelBuildID:        BuildID(build),
					LabelBuild:          build.Name,
					LabelBuildNamespace: build.Namespace,
					LabelVM:             vmSpec.Name,
				},
			},
			"spec": spec,
		},
	}

	if err := d.Client.Create(ctx, dv); err != nil {
		if errors.IsAlreadyExists(err) {
			return v1alpha1.VMPhaseDiskCaptured, nil
		}
		return v1alpha1.VMPhaseFailed, fmt.Errorf("creating output DataVolume: %w", err)
	}

	return v1alpha1.VMPhaseDiskCaptured, nil
}
