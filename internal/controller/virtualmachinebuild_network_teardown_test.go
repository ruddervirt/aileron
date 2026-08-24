/*
Copyright 2026.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.
*/

package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	teardownTestVPCGVK    = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Vpc"}
	teardownTestSubnetGVK = schema.GroupVersionKind{Group: "kubeovn.io", Version: "v1", Kind: "Subnet"}
)

// networkTeardownScheme registers the aileron CRDs plus the (cluster-scoped)
// KubeOVN Vpc/Subnet kinds teardownBuildNetworkAndVMNS touches, so the fake
// client can serve unstructured List/Get/Delete for them.
func networkTeardownScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, gvk := range []schema.GroupVersionKind{teardownTestVPCGVK, teardownTestSubnetGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		listGVK := gvk
		listGVK.Kind += "List"
		s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return s
}

func kubeOVNObj(gvk schema.GroupVersionKind, name string, labels map[string]string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	o.SetName(name)
	o.SetLabels(labels)
	return o
}

// TestTeardownBuildNetworkAndVMNS_DeletesNetworkAndVMNS is the core regression
// test for the failed-build network leak: given a build's labeled VPC, subnet,
// and VirtualMachineNamespace root, teardownBuildNetworkAndVMNS must delete all
// three and report requeue=false. This is the function that closes the gap
// where a Failed build's network topology and VMNS root previously leaked
// forever, since nothing else ever tore them down for a build whose CR is never
// deleted (only handleDeletion's ordered teardown did, which only runs on CR
// deletion).
func TestTeardownBuildNetworkAndVMNS_DeletesNetworkAndVMNS(t *testing.T) {
	buildID := "vm-failed1"
	sel := map[string]string{build.LabelBuildID: buildID}

	vpc := kubeOVNObj(teardownTestVPCGVK, buildID+"-default-vpc", sel)
	subnet := kubeOVNObj(teardownTestSubnetGVK, buildID+"-default-subnet", sel)
	vmns := &v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: buildID, Namespace: reaperNS},
	}

	c := fake.NewClientBuilder().WithScheme(networkTeardownScheme(t)).WithObjects(vpc, subnet, vmns).Build()
	r := &VirtualMachineBuildReconciler{Client: c}

	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: reaperNS},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:                 buildID,
			BuildNamespace:          reaperNS,
			VirtualMachineNamespace: buildID,
		},
	}

	requeue, err := r.teardownBuildNetworkAndVMNS(context.Background(), vmBuild)
	if err != nil {
		t.Fatalf("teardownBuildNetworkAndVMNS: %v", err)
	}
	if requeue {
		t.Fatal("requeue = true, want false once teardown completes")
	}

	gotVPC := &unstructured.Unstructured{}
	gotVPC.SetGroupVersionKind(teardownTestVPCGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: buildID + "-default-vpc"}, gotVPC); err == nil {
		t.Error("VPC still exists after teardown")
	}

	gotSubnet := &unstructured.Unstructured{}
	gotSubnet.SetGroupVersionKind(teardownTestSubnetGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: buildID + "-default-subnet"}, gotSubnet); err == nil {
		t.Error("subnet still exists after teardown")
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: buildID, Namespace: reaperNS}, &v1alpha1.VirtualMachineNamespace{}); err == nil {
		t.Error("VirtualMachineNamespace still exists after teardown")
	}
}

// TestTeardownBuildNetworkAndVMNS_LeavesOtherBuildsAlone confirms the label
// selector is scoped to the given build — a sibling build's VPC (and VMNS,
// referenced by a build with a different Status.VirtualMachineNamespace) must
// survive.
func TestTeardownBuildNetworkAndVMNS_LeavesOtherBuildsAlone(t *testing.T) {
	mine := map[string]string{build.LabelBuildID: "vm-mine"}
	theirs := map[string]string{build.LabelBuildID: "vm-other"}

	myVPC := kubeOVNObj(teardownTestVPCGVK, "vm-mine-vpc", mine)
	otherVPC := kubeOVNObj(teardownTestVPCGVK, "vm-other-vpc", theirs)
	otherVMNS := &v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-other", Namespace: reaperNS},
	}

	c := fake.NewClientBuilder().WithScheme(networkTeardownScheme(t)).WithObjects(myVPC, otherVPC, otherVMNS).Build()
	r := &VirtualMachineBuildReconciler{Client: c}

	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld-mine", Namespace: reaperNS},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:                 "vm-mine",
			BuildNamespace:          reaperNS,
			VirtualMachineNamespace: "", // no VMNS of its own in this fixture
		},
	}

	if _, err := r.teardownBuildNetworkAndVMNS(context.Background(), vmBuild); err != nil {
		t.Fatalf("teardownBuildNetworkAndVMNS: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(teardownTestVPCGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "vm-other-vpc"}, got); err != nil {
		t.Error("other build's VPC was deleted but should survive")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "vm-other", Namespace: reaperNS}, &v1alpha1.VirtualMachineNamespace{}); err != nil {
		t.Error("other build's VirtualMachineNamespace was deleted but should survive")
	}
}
