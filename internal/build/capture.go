// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DiskCapturer handles the DiskCaptured phase for a single VM: cloning every
// one of its build disks — boot and secondary alike — into its own output
// DataVolume. Capturing secondary disks (not just the boot disk) lets a
// buildRef child clone a parent's data disks the same way it clones the
// parent's boot disk (see resolveBuildRef in source.go).
type DiskCapturer struct {
	Client client.Client
}

func (d *DiskCapturer) HandleVM(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (v1alpha1.VMPhase, error) {
	// Output DVs are created in the build namespace (which becomes the template namespace).
	outputNS := BuildNS(build)
	srcNS := build.Status.BuildNamespace
	if srcNS == "" {
		srcNS = build.Namespace
	}

	allSucceeded := true

	// Boot disk.
	bootOutputName := BuildNameForOutputDV(BuildID(build), vmSpec.Name)
	bootSrcName := BuildNameForBuildVMDataVolume(BuildID(build), vmSpec.Name)
	succeeded, err := d.ensureOutputDV(ctx, build, vmSpec, outputNS, bootOutputName, bootSrcName, srcNS, BootDisk(vmSpec).Size)
	if err != nil {
		return v1alpha1.VMPhaseFailed, fmt.Errorf("capturing boot disk: %w", err)
	}
	if succeeded {
		vmStatus.OutputDataVolume = fmt.Sprintf("%s/%s", outputNS, bootOutputName)
	} else {
		allSucceeded = false
	}

	// Secondary (non-boot) disks.
	var outputVolumes []v1alpha1.DiskOutputVolume
	for i, disk := range DefaultDisks(vmSpec) {
		if i == 0 {
			continue
		}
		srcName := DiskDVName(BuildID(build), vmSpec.Name, i, disk.Name)
		outputName := BuildNameForOutputDiskDV(BuildID(build), vmSpec.Name, disk.Name)
		succeeded, err := d.ensureOutputDV(ctx, build, vmSpec, outputNS, outputName, srcName, srcNS, disk.Size)
		if err != nil {
			return v1alpha1.VMPhaseFailed, fmt.Errorf("capturing disk %s: %w", disk.Name, err)
		}
		if succeeded {
			outputVolumes = append(outputVolumes, v1alpha1.DiskOutputVolume{
				Name:       disk.Name,
				DataVolume: fmt.Sprintf("%s/%s", outputNS, outputName),
			})
		} else {
			allSucceeded = false
		}
	}
	if outputVolumes != nil {
		vmStatus.OutputDataVolumes = outputVolumes
	}

	if !allSucceeded {
		return v1alpha1.VMPhaseDiskCaptured, nil
	}
	return v1alpha1.VMPhaseSucceeded, nil
}

// ensureOutputDV ensures a captured output DataVolume exists at
// outputNS/outputName, cloned from the per-build source DV at srcNS/srcName.
// Returns true once the output DV has reached Succeeded.
func (d *DiskCapturer) ensureOutputDV(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, outputNS, outputName, srcName, srcNS string, requestedSize resource.Quantity) (bool, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(dvGVK)
	err := d.Client.Get(ctx, types.NamespacedName{Name: outputName, Namespace: outputNS}, existing)
	if err == nil {
		phase, _, _ := unstructured.NestedString(existing.Object, "status", "phase")
		switch phase {
		case PhaseSucceeded:
			return true, nil
		case PhaseFailed:
			return false, fmt.Errorf("output DataVolume %s clone failed for VM %s", outputName, vmSpec.Name)
		default:
			return false, nil
		}
	}
	if !errors.IsNotFound(err) {
		return false, fmt.Errorf("checking output DataVolume %s: %w", outputName, err)
	}

	// Use the actual source PVC size — the on-disk PVC may be larger than
	// the spec's requested size due to filesystem overhead, CDI rounding,
	// or Rook Ceph minimum allocation. CDI rejects clones where target < source.
	logger := logf.FromContext(ctx)
	diskSize := requestedSize
	srcPVC := &corev1.PersistentVolumeClaim{}
	if err := d.Client.Get(ctx, types.NamespacedName{Name: srcName, Namespace: srcNS}, srcPVC); err == nil {
		if actual := srcPVC.Status.Capacity.Storage(); actual != nil && actual.Cmp(diskSize) > 0 {
			logger.Info("Output DV: using source PVC actual size instead of spec size",
				"vm", vmSpec.Name, "dv", outputName, "specSize", diskSize.String(), "actualSize", actual.String())
			diskSize = *actual
		}
	}

	spec := map[string]any{
		"source": map[string]any{
			"pvc": map[string]any{
				"name":      srcName,
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
				"name":      outputName,
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

	if err := d.Client.Create(ctx, dv); err != nil && !errors.IsAlreadyExists(err) {
		return false, fmt.Errorf("creating output DataVolume %s: %w", outputName, err)
	}
	return false, nil
}
