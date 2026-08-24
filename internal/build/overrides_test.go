// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"reflect"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

func TestEffectiveSubnetUnmanaged(t *testing.T) {
	build := func(overrides *v1alpha1.BuildOverrides) *v1alpha1.VirtualMachineBuild {
		return &v1alpha1.VirtualMachineBuild{
			Spec: v1alpha1.VirtualMachineBuildSpec{
				Network: &v1alpha1.Network{
					Subnets: []v1alpha1.Subnet{
						{Name: "lan", CIDR: "192.168.1.0/24", Unmanaged: true},
						{Name: "mgmt", CIDR: "10.0.1.0/24"},
					},
				},
				BuildOverrides: overrides,
			},
		}
	}

	tests := []struct {
		name      string
		subnet    string
		overrides *v1alpha1.BuildOverrides
		want      bool
	}{
		{name: "base unmanaged, no override", subnet: "lan", want: true},
		{name: "base managed, no override", subnet: "mgmt", want: false},
		{
			name:      "override unmanaged->managed for build",
			subnet:    "lan",
			overrides: &v1alpha1.BuildOverrides{Subnets: []v1alpha1.SubnetOverride{{Name: "lan", Unmanaged: new(false)}}},
			want:      false,
		},
		{
			name:      "override for a different subnet leaves base",
			subnet:    "lan",
			overrides: &v1alpha1.BuildOverrides{Subnets: []v1alpha1.SubnetOverride{{Name: "mgmt", Unmanaged: new(true)}}},
			want:      true,
		},
		{
			name:      "override with nil unmanaged leaves base",
			subnet:    "lan",
			overrides: &v1alpha1.BuildOverrides{Subnets: []v1alpha1.SubnetOverride{{Name: "lan"}}},
			want:      true,
		},
		{
			name:      "override can also force managed->unmanaged",
			subnet:    "mgmt",
			overrides: &v1alpha1.BuildOverrides{Subnets: []v1alpha1.SubnetOverride{{Name: "mgmt", Unmanaged: new(true)}}},
			want:      true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveSubnetUnmanaged(build(tc.overrides), tc.subnet); got != tc.want {
				t.Errorf("effectiveSubnetUnmanaged(%q) = %v, want %v", tc.subnet, got, tc.want)
			}
		})
	}
}

func TestEffectiveVMNICs(t *testing.T) {
	baseNICs := []v1alpha1.VMNIC{
		{Name: "mgmt", Subnet: "lab"},
		{Name: "lan", Subnet: "lab"},
	}
	overrideNICs := []v1alpha1.VMNIC{
		{Name: "mgmt", Subnet: "lab"},
		{Name: "lan", Subnet: "lab"},
		{Name: "net", Subnet: "internet"},
	}
	vmSpec := &v1alpha1.BuildVM{Name: "builder", NICs: baseNICs}

	tests := []struct {
		name      string
		overrides *v1alpha1.BuildOverrides
		want      []v1alpha1.VMNIC
	}{
		{
			name:      "no overrides → base",
			overrides: nil,
			want:      baseNICs,
		},
		{
			name: "override matches VM → override",
			overrides: &v1alpha1.BuildOverrides{VMs: []v1alpha1.BuildVMOverride{
				{Name: "builder", NICs: overrideNICs},
			}},
			want: overrideNICs,
		},
		{
			name: "override for different VM → base",
			overrides: &v1alpha1.BuildOverrides{VMs: []v1alpha1.BuildVMOverride{
				{Name: "other-vm", NICs: overrideNICs},
			}},
			want: baseNICs,
		},
		{
			name: "override entry without nics → base",
			overrides: &v1alpha1.BuildOverrides{VMs: []v1alpha1.BuildVMOverride{
				{Name: "builder"},
			}},
			want: baseNICs,
		},
		{
			name: "override with empty nics slice → base",
			overrides: &v1alpha1.BuildOverrides{VMs: []v1alpha1.BuildVMOverride{
				{Name: "builder", NICs: []v1alpha1.VMNIC{}},
			}},
			want: baseNICs,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			build := &v1alpha1.VirtualMachineBuild{
				Spec: v1alpha1.VirtualMachineBuildSpec{
					BuildOverrides: tc.overrides,
				},
			}
			got := effectiveVMNICs(build, vmSpec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("effectiveVMNICs = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestEffectiveVMNICsIdentityInheritance covers the identity-field invariant:
// an override NIC sharing a name with a base NIC keeps the base NIC's mac,
// slot, and model, because guest state (e.g. Windows DHCP reservations keyed
// to the MAC) is baked during the build and clones boot with the base values.
func TestEffectiveVMNICsIdentityInheritance(t *testing.T) {
	tests := []struct {
		name     string
		base     []v1alpha1.VMNIC
		override []v1alpha1.VMNIC
		want     []v1alpha1.VMNIC
	}{
		{
			name: "conflicting mac/slot/model → base wins",
			base: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:aa:aa:aa", Slot: 2, Model: "e1000"},
			},
			override: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:bb:bb:bb", Slot: 3, Model: "virtio"},
				{Name: "nic1", Subnet: "default", MAC: "52:54:00:cc:cc:cc", Slot: 1, Model: "e1000"},
			},
			want: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:aa:aa:aa", Slot: 2, Model: "e1000"},
				{Name: "nic1", Subnet: "default", MAC: "52:54:00:cc:cc:cc", Slot: 1, Model: "e1000"},
			},
		},
		{
			name: "empty override fields inherit from base",
			base: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:aa:aa:aa", Slot: 2, Model: "e1000"},
			},
			override: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc"},
			},
			want: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:aa:aa:aa", Slot: 2, Model: "e1000"},
			},
		},
		{
			name: "empty base fields leave override values",
			base: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc"},
			},
			override: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:bb:bb:bb", Slot: 3, Model: "virtio"},
			},
			want: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:bb:bb:bb", Slot: 3, Model: "virtio"},
			},
		},
		{
			name: "override-only NIC untouched, subnet/ip stay overridable",
			base: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "dc", MAC: "52:54:00:aa:aa:aa", Slot: 2},
			},
			override: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "provision", IP: "10.9.0.5", MAC: "52:54:00:bb:bb:bb", Slot: 2},
				{Name: "mgmt", Subnet: "provision", MAC: "52:54:00:dd:dd:dd", Slot: 4},
			},
			want: []v1alpha1.VMNIC{
				{Name: "nic2", Subnet: "provision", IP: "10.9.0.5", MAC: "52:54:00:aa:aa:aa", Slot: 2},
				{Name: "mgmt", Subnet: "provision", MAC: "52:54:00:dd:dd:dd", Slot: 4},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vmSpec := &v1alpha1.BuildVM{Name: "builder", NICs: tc.base}
			build := &v1alpha1.VirtualMachineBuild{
				Spec: v1alpha1.VirtualMachineBuildSpec{
					BuildOverrides: &v1alpha1.BuildOverrides{VMs: []v1alpha1.BuildVMOverride{
						{Name: "builder", NICs: tc.override},
					}},
				},
			}
			// Snapshot to prove inheriting identity fields never mutates the
			// authored override spec in place.
			origMAC := tc.override[0].MAC
			got := effectiveVMNICs(build, vmSpec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("effectiveVMNICs = %#v, want %#v", got, tc.want)
			}
			if build.Spec.BuildOverrides.VMs[0].NICs[0].MAC != origMAC {
				t.Errorf("effectiveVMNICs mutated the override spec in place")
			}
		})
	}
}
