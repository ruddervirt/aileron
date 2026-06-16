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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aileroniov1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

var _ = Describe("VirtualMachineBuild Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		virtualmachinebuild := &aileroniov1alpha1.VirtualMachineBuild{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind VirtualMachineBuild")
			err := k8sClient.Get(ctx, typeNamespacedName, virtualmachinebuild)
			if err != nil && errors.IsNotFound(err) {
				resource := &aileroniov1alpha1.VirtualMachineBuild{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: aileroniov1alpha1.VirtualMachineBuildSpec{
						VMs: []aileroniov1alpha1.BuildVM{
							{
								Name: "test-vm",
								Source: aileroniov1alpha1.BuildSource{
									ContainerDisk: "quay.io/containerdisks/ubuntu:22.04",
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &aileroniov1alpha1.VirtualMachineBuild{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance VirtualMachineBuild")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &VirtualMachineBuildReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})

// buildWithNICs returns a minimal VirtualMachineBuild whose pendingHandler.Handle
// will exercise the network validator: one VM with a containerDisk source, one
// VPC, the given subnets and NICs.
func buildWithNICs(subnets []aileroniov1alpha1.Subnet, nics []aileroniov1alpha1.VMNIC) *aileroniov1alpha1.VirtualMachineBuild {
	return &aileroniov1alpha1.VirtualMachineBuild{
		Spec: aileroniov1alpha1.VirtualMachineBuildSpec{
			Network: &aileroniov1alpha1.Network{
				VPCs:    []aileroniov1alpha1.VPC{{Name: "default"}},
				Subnets: subnets,
			},
			VMs: []aileroniov1alpha1.BuildVM{
				{
					Name: "dc",
					Source: aileroniov1alpha1.BuildSource{
						ContainerDisk: "quay.io/containerdisks/ubuntu:22.04",
					},
					NICs: nics,
				},
			},
		},
	}
}

var _ = Describe("pendingHandler network validation", func() {
	ctx := context.Background()

	It("fails the build when a NIC IP falls outside its subnet CIDR", func() {
		// Matches the rudderusa-deploy bug: eth0 on mgmt subnet, mgmt is
		// 10.0.99.0/24, but IP 10.0.1.10 was injected — kube-ovn rejects
		// with AddressOutOfRange and the launcher pod hangs in Init:0/1.
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{Name: "mgmt", VPC: "default", CIDR: "10.0.99.0/24"}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "mgmt", IP: "10.0.1.10"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseFailed))
		Expect(err).To(MatchError(ContainSubstring("IP 10.0.1.10 is not within subnet \"mgmt\" CIDR 10.0.99.0/24")))
	})

	It("accepts a NIC IP that is inside its subnet CIDR", func() {
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{Name: "mgmt", VPC: "default", CIDR: "10.0.99.0/24"}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "mgmt", IP: "10.0.99.10"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseNetworking))
	})

	It("accepts a NIC with no static IP (DHCP)", func() {
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{Name: "mgmt", VPC: "default", CIDR: "10.0.99.0/24"}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "mgmt"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseNetworking))
	})

	It("accepts an unmanaged subnet with a static NIC IP inside the CIDR", func() {
		// Mirrors hack/dc.yaml: lab subnet is unmanaged (guest serves DHCP).
		// The DC's NIC has a static IP reservation inside the CIDR.
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{
				Name: "lab", VPC: "default", CIDR: "10.0.1.0/24", Unmanaged: true,
			}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth1", Subnet: "lab", IP: "10.0.1.10"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(err).NotTo(HaveOccurred())
		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseNetworking))
	})

	It("fails the build when an unmanaged subnet sets dns", func() {
		// dns is only meaningful when OVN serves DHCP; on unmanaged segments
		// the guest gateway owns DHCP/DNS, so a dns value is a config mistake.
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{
				Name: "lan", VPC: "default", CIDR: "192.168.1.0/24", Unmanaged: true, DNS: "192.168.1.1",
			}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "lan"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseFailed))
		Expect(err).To(MatchError(ContainSubstring("dns has no effect on unmanaged subnets")))
	})

	It("fails the build when an unmanaged subnet's CIDR is narrower than /29", func() {
		// ApplyUnmanaged parks OVN's mandatory gateway router port on the
		// second-to-last usable IP; a /30 leaves it colliding with the guest
		// gateway at the first usable IP.
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{
				Name: "lan", VPC: "default", CIDR: "192.168.1.0/30", Unmanaged: true,
			}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "lan"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseFailed))
		Expect(err).To(MatchError(ContainSubstring("too small")))
	})

	It("fails the build when a NIC IP is not a valid address", func() {
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{Name: "mgmt", VPC: "default", CIDR: "10.0.99.0/24"}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "mgmt", IP: "not-an-ip"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseFailed))
		Expect(err).To(MatchError(ContainSubstring("has invalid IP \"not-an-ip\"")))
	})

	It("fails the build when a managed subnet has an invalid CIDR", func() {
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{Name: "mgmt", VPC: "default", CIDR: "10.0.99.0"}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "mgmt", IP: "10.0.99.10"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseFailed))
		Expect(err).To(MatchError(ContainSubstring("subnet \"mgmt\" has invalid CIDR \"10.0.99.0\"")))
	})

	It("fails the build when a NIC references an unknown subnet", func() {
		build := buildWithNICs(
			[]aileroniov1alpha1.Subnet{{Name: "mgmt", VPC: "default", CIDR: "10.0.99.0/24"}},
			[]aileroniov1alpha1.VMNIC{{Name: "eth0", Subnet: "ghost"}},
		)
		h := &pendingHandler{client: k8sClient}

		phase, err := h.Handle(ctx, build)

		Expect(phase).To(Equal(aileroniov1alpha1.BuildPhaseFailed))
		Expect(err).To(MatchError(ContainSubstring("references unknown subnet \"ghost\"")))
	})
})

var _ = Describe("normalizeBuildRefDisks", func() {
	ctx := context.Background()
	var r *VirtualMachineBuildReconciler

	BeforeEach(func() {
		r = &VirtualMachineBuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	// createParent persists a parent build (in the default namespace) whose
	// single VM carries the given disks, and registers it for cleanup.
	createParent := func(name string, disks []aileroniov1alpha1.BuildDisk) {
		parent := &aileroniov1alpha1.VirtualMachineBuild{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: aileroniov1alpha1.VirtualMachineBuildSpec{
				VMs: []aileroniov1alpha1.BuildVM{{
					Name:   "base",
					Source: aileroniov1alpha1.BuildSource{Blank: true},
					Disks:  disks,
				}},
			},
		}
		Expect(k8sClient.Create(ctx, parent)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, parent)).To(Succeed())
		})
	}

	disk := func(name, size, bus string) aileroniov1alpha1.BuildDisk {
		return aileroniov1alpha1.BuildDisk{Name: name, Size: resource.MustParse(size), Bus: bus}
	}

	Context("buildRef sources", func() {
		It("prepends the parent's boot disk so listed disks become additional", func() {
			createParent("parent-extra", []aileroniov1alpha1.BuildDisk{disk("rootdisk", "25Gi", "virtio")})

			child := &aileroniov1alpha1.BuildVM{
				Name: "server",
				Source: aileroniov1alpha1.BuildSource{
					BuildRef: &aileroniov1alpha1.BuildReference{Name: "parent-extra", Namespace: "default"},
				},
				Disks: []aileroniov1alpha1.BuildDisk{disk("supplemental", "5Gi", "virtio")},
			}

			changed, err := r.normalizeBuildRefDisks(ctx, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			Expect(child.Disks).To(HaveLen(2))
			Expect(child.Disks[0].Name).To(Equal("rootdisk"))
			Expect(child.Disks[0].Size.String()).To(Equal("25Gi"))
			Expect(child.Disks[1].Name).To(Equal("supplemental"))
			Expect(child.Disks[1].Size.String()).To(Equal("5Gi"))
		})

		It("inherits the parent's boot disk bus", func() {
			createParent("parent-scsi", []aileroniov1alpha1.BuildDisk{disk("rootdisk", "40Gi", "scsi")})

			child := &aileroniov1alpha1.BuildVM{
				Name: "server",
				Source: aileroniov1alpha1.BuildSource{
					BuildRef: &aileroniov1alpha1.BuildReference{Name: "parent-scsi", Namespace: "default"},
				},
			}

			changed, err := r.normalizeBuildRefDisks(ctx, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			Expect(child.Disks).To(HaveLen(1))
			Expect(child.Disks[0].Name).To(Equal("rootdisk"))
			Expect(child.Disks[0].Bus).To(Equal("scsi"))
		})

		It("defaults the boot disk when the parent specifies none", func() {
			createParent("parent-nodisks", nil)

			child := &aileroniov1alpha1.BuildVM{
				Name: "server",
				Source: aileroniov1alpha1.BuildSource{
					BuildRef: &aileroniov1alpha1.BuildReference{Name: "parent-nodisks", Namespace: "default"},
				},
				Disks: []aileroniov1alpha1.BuildDisk{disk("data", "5Gi", "virtio")},
			}

			changed, err := r.normalizeBuildRefDisks(ctx, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
			Expect(child.Disks).To(HaveLen(2))
			Expect(child.Disks[0].Name).To(Equal("rootdisk"))
			Expect(child.Disks[0].Size.String()).To(Equal("20Gi"))
			Expect(child.Disks[1].Name).To(Equal("data"))
		})

		It("errors when a listed disk collides with the inherited boot disk name", func() {
			createParent("parent-collide", []aileroniov1alpha1.BuildDisk{disk("rootdisk", "25Gi", "virtio")})

			child := &aileroniov1alpha1.BuildVM{
				Name: "server",
				Source: aileroniov1alpha1.BuildSource{
					BuildRef: &aileroniov1alpha1.BuildReference{Name: "parent-collide", Namespace: "default"},
				},
				Disks: []aileroniov1alpha1.BuildDisk{disk("rootdisk", "5Gi", "virtio")},
			}

			changed, err := r.normalizeBuildRefDisks(ctx, child)
			Expect(err).To(MatchError(ContainSubstring("collides with inherited boot disk")))
			Expect(changed).To(BeFalse())
		})
	})

	Context("non-buildRef sources", func() {
		It("leaves disks unchanged for a containerDisk source", func() {
			child := &aileroniov1alpha1.BuildVM{
				Name: "server",
				Source: aileroniov1alpha1.BuildSource{
					ContainerDisk: "quay.io/containerdisks/ubuntu:22.04",
				},
				Disks: []aileroniov1alpha1.BuildDisk{disk("rootdisk", "20Gi", "virtio")},
			}

			changed, err := r.normalizeBuildRefDisks(ctx, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse())
			Expect(child.Disks).To(HaveLen(1))
			Expect(child.Disks[0].Name).To(Equal("rootdisk"))
		})

		It("leaves disks unchanged for a blank source with additional disks", func() {
			child := &aileroniov1alpha1.BuildVM{
				Name:   "server",
				Source: aileroniov1alpha1.BuildSource{Blank: true},
				Disks: []aileroniov1alpha1.BuildDisk{
					disk("rootdisk", "20Gi", "virtio"),
					disk("data", "5Gi", "virtio"),
				},
			}

			changed, err := r.normalizeBuildRefDisks(ctx, child)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeFalse())
			Expect(child.Disks).To(HaveLen(2))
			Expect(child.Disks[0].Name).To(Equal("rootdisk"))
			Expect(child.Disks[1].Name).To(Equal("data"))
		})
	})
})
