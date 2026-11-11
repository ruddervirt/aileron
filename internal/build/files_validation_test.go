// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"strings"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

func TestValidateFiles(t *testing.T) {
	tests := []struct {
		name string
		spec v1alpha1.VirtualMachineBuildSpec
		want string
	}{
		{name: "no files or references"},
		{
			name: "missing HTTP file",
			spec: v1alpha1.VirtualMachineBuildSpec{HttpDirectory: &v1alpha1.HttpDirectory{Files: []v1alpha1.HttpDirectoryFileRef{{Name: "preseed.cfg"}}}},
			want: `spec.httpDirectory.files[0]: file "preseed.cfg" not found in spec.files`,
		},
		{
			name: "missing floppy file",
			spec: v1alpha1.VirtualMachineBuildSpec{VMs: []v1alpha1.BuildVM{{Name: "one"}, {Name: "two", Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "answer.xml"}}}}}},
			want: `spec.vms[1] (two).floppy.files[0]: file "answer.xml" not found in spec.files`,
		},
		{
			name: "missing provisioner file",
			spec: v1alpha1.VirtualMachineBuildSpec{VMs: []v1alpha1.BuildVM{{Name: "one", Provisioners: []v1alpha1.BuildProvisioner{{Type: v1alpha1.ProvisionerTypeFile, File: &v1alpha1.FileProvisioner{FileRef: "script.sh"}}}}}},
			want: `spec.vms[0] (one).provisioners[0].file.fileRef: file "script.sh" not found in spec.files`,
		},
		{
			name: "empty name", spec: v1alpha1.VirtualMachineBuildSpec{Files: []v1alpha1.BuildFile{{Inline: "content"}}},
			want: "spec.files[0].name must not be empty",
		},
		{
			name: "duplicate names", spec: v1alpha1.VirtualMachineBuildSpec{Files: []v1alpha1.BuildFile{{Name: "a", Inline: "content"}, {Name: "a", URL: "https://example.com/a"}}},
			want: `spec.files[1]: duplicate file name "a"`,
		},
		{
			name: "no content", spec: v1alpha1.VirtualMachineBuildSpec{Files: []v1alpha1.BuildFile{{Name: "a"}}},
			want: "exactly one of inline or url must be set",
		},
		{
			name: "ambiguous content", spec: v1alpha1.VirtualMachineBuildSpec{Files: []v1alpha1.BuildFile{{Name: "a", Inline: "content", URL: "https://example.com/a"}}},
			want: "exactly one of inline or url must be set",
		},
		{
			name: "valid inline and URL references",
			spec: v1alpha1.VirtualMachineBuildSpec{
				Files:         []v1alpha1.BuildFile{{Name: "a", Inline: "content"}, {Name: "b", URL: "https://example.com/b"}},
				HttpDirectory: &v1alpha1.HttpDirectory{Files: []v1alpha1.HttpDirectoryFileRef{{Name: "a"}, {Name: "b"}}},
				VMs: []v1alpha1.BuildVM{{Name: "one", Floppy: &v1alpha1.Floppy{Files: []v1alpha1.FloppyFileRef{{Name: "b"}}}, Provisioners: []v1alpha1.BuildProvisioner{
					{Type: v1alpha1.ProvisionerTypeFile, File: &v1alpha1.FileProvisioner{FileRef: "a"}},
					{Type: v1alpha1.ProvisionerTypeFile, File: &v1alpha1.FileProvisioner{}}, // external source, not a named-file reference
				}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateFiles(&v1alpha1.VirtualMachineBuild{Spec: tt.spec})
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateFiles = %v, want %q", err, tt.want)
			}
		})
	}
}
