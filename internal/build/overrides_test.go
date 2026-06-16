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
