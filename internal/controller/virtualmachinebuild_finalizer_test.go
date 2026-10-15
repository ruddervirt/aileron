// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"github.com/ruddervirt/aileron/internal/build"
)

// subnetItemsSchemaPath addresses the Subnet item schema inside the
// VirtualMachineBuild CRD's openAPIV3Schema (spec.network.subnets[]).
var subnetItemsSchemaPath = []string{
	"schema", "openAPIV3Schema", "properties", "spec", "properties",
	"network", "properties", "subnets", "items",
}

// editSubnetItemsSchema applies edit to the Subnet item schema of the live
// VirtualMachineBuild CRD and writes the CRD back. The API server picks the
// change up asynchronously, so callers must poll for the effect.
func editSubnetItemsSchema(ctx context.Context, edit func(items map[string]any)) {
	GinkgoHelper()
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition",
	})
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "virtualmachinebuilds.ruddervirt.io"}, crd)).To(Succeed())

	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	Expect(versions).To(HaveLen(1))
	version := versions[0].(map[string]any)

	items, found, err := unstructured.NestedMap(version, subnetItemsSchemaPath...)
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	edit(items)
	Expect(unstructured.SetNestedMap(version, items, subnetItemsSchemaPath...)).To(Succeed())
	versions[0] = version
	Expect(unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")).To(Succeed())
	Expect(k8sClient.Update(ctx, crd)).To(Succeed())
}

// legacyBuild returns a VirtualMachineBuild, already carrying the cleanup
// finalizer, whose second subnet is shaped like the pre-tightening CRs found
// in production: unmanaged, DHCP owned by the guest, and no usable cidr.
// cidr is nil to omit the field entirely, or a string to store verbatim.
func legacyBuild(name string, cidr any) *unstructured.Unstructured {
	dcSubnet := map[string]any{"name": "dc", "unmanaged": true, "dhcp": true}
	if cidr != nil {
		dcSubnet["cidr"] = cidr
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ruddervirt.io/v1alpha1",
		"kind":       "VirtualMachineBuild",
		"metadata": map[string]any{
			"name":       name,
			"namespace":  "default",
			"finalizers": []any{finalizerName},
		},
		"spec": map[string]any{
			"vms": []any{map[string]any{
				"name":      "dc",
				"source":    map[string]any{"containerDisk": "quay.io/containerdisks/ubuntu:22.04"},
				"resources": map[string]any{"cpu": "1", "memory": "1Gi"},
			}},
			"network": map[string]any{
				"subnets": []any{
					map[string]any{"name": "lan", "cidr": "10.0.1.0/24"},
					dcSubnet,
				},
			},
		},
	}}
}

// Regression test for the immortal-CR bug: a build created before the CRD
// schema required a well-formed subnet cidr could not be deleted, because the
// finalizer-removal write was rejected by validation of the (now invalid)
// stored spec. Cleanup had already run, so the CR sat with a deletion
// timestamp forever. The finalizer path must not depend on the stored spec
// being valid under the current schema.
var _ = Describe("VirtualMachineBuild finalizer removal", func() {
	ctx := context.Background()

	// storeLegacyBuild persists obj even though it violates the current
	// schema, by temporarily relaxing the live CRD the way the pre-tightening
	// schema looked (no cidr pattern, cidr not required), then restores the
	// schema and waits until the API server enforces it again.
	storeLegacyBuild := func(obj *unstructured.Unstructured) {
		GinkgoHelper()
		relax := func(items map[string]any) {
			unstructured.RemoveNestedField(items, "properties", "cidr", "pattern")
			Expect(unstructured.SetNestedStringSlice(items, []string{"name"}, "required")).To(Succeed())
		}
		restore := func(items map[string]any) {
			Expect(unstructured.SetNestedField(items,
				`^([0-9]{1,3}\.){3}[0-9]{1,3}/[0-9]{1,2}$`, "properties", "cidr", "pattern")).To(Succeed())
			Expect(unstructured.SetNestedStringSlice(items, []string{"cidr", "name"}, "required")).To(Succeed())
		}

		editSubnetItemsSchema(ctx, relax)
		DeferCleanup(func() { editSubnetItemsSchema(ctx, restore) })
		Eventually(func() error {
			return k8sClient.Create(ctx, obj.DeepCopy())
		}).WithTimeout(10*time.Second).Should(Succeed(), "relaxed schema should accept the legacy build")

		editSubnetItemsSchema(ctx, restore)
		Eventually(func() error {
			probe := legacyBuild(obj.GetName()+"-probe", "")
			probe.SetFinalizers(nil)
			err := k8sClient.Create(ctx, probe)
			if err == nil {
				_ = k8sClient.Delete(ctx, probe)
				return fmt.Errorf("schema not yet re-tightened")
			}
			if !errors.IsInvalid(err) {
				return err
			}
			return nil
		}).WithTimeout(10*time.Second).Should(Succeed(), "restored schema should reject an empty cidr again")
	}

	deleteAndReconcile := func(name string) {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: "default"}
		stored := &v1alpha1.VirtualMachineBuild{}
		Expect(k8sClient.Get(ctx, key, stored)).To(Succeed())
		Expect(k8sClient.Delete(ctx, stored)).To(Succeed())

		r := &VirtualMachineBuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			return errors.IsNotFound(k8sClient.Get(ctx, key, &v1alpha1.VirtualMachineBuild{}))
		}).WithTimeout(10*time.Second).Should(BeTrue(), "build should be gone once the finalizer is removed")
	}

	It("deletes a build whose stored spec omits a now-required subnet cidr", func() {
		obj := legacyBuild("legacy-no-cidr", nil)
		storeLegacyBuild(obj)
		deleteAndReconcile(obj.GetName())
	})

	It("deletes a build whose stored spec has an empty subnet cidr", func() {
		obj := legacyBuild("legacy-empty-cidr", "")
		storeLegacyBuild(obj)
		deleteAndReconcile(obj.GetName())
	})

	It("deletes a valid build through the same path", func() {
		obj := legacyBuild("legacy-valid", "10.0.2.0/24")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		deleteAndReconcile(obj.GetName())
	})
})

// TestHandleDeletion_ValidBuildRunsFullCleanupBeforeFinalizerRemoval is the
// inverse guard for the finalizer fix: on a schema-valid build the cleanup
// semantics are unchanged — every labeled resource, the KubeOVN topology, and
// the VirtualMachineNamespace are torn down, and only then is the finalizer
// removed so the CR disappears.
func TestHandleDeletion_ValidBuildRunsFullCleanupBeforeFinalizerRemoval(t *testing.T) {
	ctx := context.Background()
	buildID := "vm-valid1"
	sel := map[string]string{build.LabelBuildID: buildID}

	s := networkTeardownScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "bld", Namespace: reaperNS, Finalizers: []string{finalizerName}},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs:     []v1alpha1.BuildVM{{Name: "dc", Source: v1alpha1.BuildSource{ContainerDisk: "quay.io/containerdisks/ubuntu:22.04"}}},
			Network: &v1alpha1.Network{Subnets: []v1alpha1.Subnet{{Name: "lan", CIDR: "10.0.1.0/24"}}},
		},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:                 buildID,
			BuildNamespace:          reaperNS,
			VirtualMachineNamespace: buildID,
		},
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: buildID + "-cloudinit", Namespace: reaperNS, Labels: sel}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: buildID + "-ssh", Namespace: reaperNS, Labels: sel}}
	vpc := kubeOVNObj(teardownTestVPCGVK, buildID+"-default-vpc", sel)
	subnet := kubeOVNObj(teardownTestSubnetGVK, buildID+"-lan", sel)
	vmns := &v1alpha1.VirtualMachineNamespace{ObjectMeta: metav1.ObjectMeta{Name: buildID, Namespace: reaperNS}}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(vmBuild, cm, secret, vpc, subnet, vmns).
		WithStatusSubresource(&v1alpha1.VirtualMachineBuild{}).
		Build()
	r := &VirtualMachineBuildReconciler{Client: c}

	if err := c.Delete(ctx, vmBuild); err != nil {
		t.Fatalf("delete build: %v", err)
	}
	deleting := &v1alpha1.VirtualMachineBuild{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(vmBuild), deleting); err != nil {
		t.Fatalf("get deleting build: %v", err)
	}
	if deleting.DeletionTimestamp.IsZero() {
		t.Fatal("expected the finalizer to hold the build in Terminating")
	}

	res, err := r.handleDeletion(ctx, deleting)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected cleanup to finish in one pass, got %+v", res)
	}

	gone := func(name string, obj client.Object, key types.NamespacedName) {
		t.Helper()
		err := c.Get(ctx, key, obj)
		if !errors.IsNotFound(err) {
			t.Errorf("%s should have been deleted, got err=%v", name, err)
		}
	}
	gone("ConfigMap", &corev1.ConfigMap{}, client.ObjectKeyFromObject(cm))
	gone("Secret", &corev1.Secret{}, client.ObjectKeyFromObject(secret))
	gone("Vpc", kubeOVNObj(teardownTestVPCGVK, "", nil), types.NamespacedName{Name: vpc.GetName()})
	gone("Subnet", kubeOVNObj(teardownTestSubnetGVK, "", nil), types.NamespacedName{Name: subnet.GetName()})
	gone("VirtualMachineNamespace", &v1alpha1.VirtualMachineNamespace{}, client.ObjectKeyFromObject(vmns))
	gone("VirtualMachineBuild", &v1alpha1.VirtualMachineBuild{}, client.ObjectKeyFromObject(vmBuild))
}
