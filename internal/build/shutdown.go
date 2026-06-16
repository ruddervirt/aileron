package build

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// VMShutdown handles the ShuttingDown phase for a single VM.
type VMShutdown struct {
	Client client.Client
}

func (s *VMShutdown) HandleVM(ctx context.Context, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM, vmStatus *v1alpha1.VMBuildStatus) (v1alpha1.VMPhase, error) {
	logger := log.FromContext(ctx)
	vmName := vmStatus.VMName
	if vmName == "" {
		vmName = BuildNameForBuildVM(BuildID(build), vmSpec.Name)
	}

	// Check if VMI still exists.
	buildNS := buildNamespace(build)
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance",
	})
	err := s.Client.Get(ctx, types.NamespacedName{Name: vmName, Namespace: buildNS}, vmi)
	if errors.IsNotFound(err) {
		return v1alpha1.VMPhaseDiskCaptured, nil
	}
	if err != nil {
		return v1alpha1.VMPhaseShuttingDown, fmt.Errorf("checking VMI: %w", err)
	}

	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	if phase == PhaseSucceeded || phase == PhaseFailed {
		return v1alpha1.VMPhaseDiskCaptured, nil
	}

	// Stop the VM.
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
	})
	err = s.Client.Get(ctx, types.NamespacedName{Name: vmName, Namespace: buildNS}, vm)
	if err != nil {
		return v1alpha1.VMPhaseShuttingDown, fmt.Errorf("getting VM for shutdown: %w", err)
	}

	runStrategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	if runStrategy != "Halted" {
		logger.Info("Stopping build VM", "vm", vmName)
		unstructured.RemoveNestedField(vm.Object, "spec", "running")
		if err := unstructured.SetNestedField(vm.Object, "Halted", "spec", "runStrategy"); err != nil {
			return v1alpha1.VMPhaseShuttingDown, fmt.Errorf("setting runStrategy=Halted: %w", err)
		}
		if err := s.Client.Update(ctx, vm); err != nil {
			return v1alpha1.VMPhaseShuttingDown, fmt.Errorf("updating VM: %w", err)
		}
	}

	return v1alpha1.VMPhaseShuttingDown, nil
}
