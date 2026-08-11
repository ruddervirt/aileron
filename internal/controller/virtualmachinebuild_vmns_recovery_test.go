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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// TestFindOwnedVMNS_IgnoresSameNameDifferentNamespace guards against a
// regression where recovering a lost Status.BuildID matched on the shared
// LabelBuild value alone (build.Name), which is not unique across tenant
// namespaces. Two VirtualMachineBuild CRs in different namespaces can share
// the same .metadata.name; recovering the wrong one poisons this build's
// status with another build's buildID/VMNS name, and downstream cleanup
// then deletes the unrelated build's resources.
func TestFindOwnedVMNS_IgnoresSameNameDifferentNamespace(t *testing.T) {
	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-base", Namespace: "team-a"},
	}

	otherTeamsVMNS := v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-other-team"},
		Spec: v1alpha1.VirtualMachineNamespaceSpec{
			OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{
				Kind:      "VirtualMachineBuild",
				Name:      "ubuntu-base",
				Namespace: "team-b",
			},
		},
	}
	ownVMNS := v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-team-a"},
		Spec: v1alpha1.VirtualMachineNamespaceSpec{
			OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{
				Kind:      "VirtualMachineBuild",
				Name:      "ubuntu-base",
				Namespace: "team-a",
			},
		},
	}

	vmnsList := &v1alpha1.VirtualMachineNamespaceList{Items: []v1alpha1.VirtualMachineNamespace{otherTeamsVMNS, ownVMNS}}

	if got := findOwnedVMNS(vmnsList, vmBuild); got != "vm-team-a" {
		t.Errorf("findOwnedVMNS() = %q, want %q (own VMNS, not the other tenant's)", got, "vm-team-a")
	}
}

// TestFindOwnedVMNS_NoMatch confirms an empty result (not a panic or a
// false match) when no listed VMNS is owned by this build — e.g. nothing
// has been created yet, or every candidate belongs to another tenant.
func TestFindOwnedVMNS_NoMatch(t *testing.T) {
	vmBuild := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-base", Namespace: "team-a"},
	}
	otherTeamsVMNS := v1alpha1.VirtualMachineNamespace{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-other-team"},
		Spec: v1alpha1.VirtualMachineNamespaceSpec{
			OwnerRef: &v1alpha1.VirtualMachineNamespaceOwnerRef{
				Kind:      "VirtualMachineBuild",
				Name:      "ubuntu-base",
				Namespace: "team-b",
			},
		},
	}
	vmnsList := &v1alpha1.VirtualMachineNamespaceList{Items: []v1alpha1.VirtualMachineNamespace{otherTeamsVMNS}}

	if got := findOwnedVMNS(vmnsList, vmBuild); got != "" {
		t.Errorf("findOwnedVMNS() = %q, want empty (no VMNS owned by this build)", got)
	}
}
