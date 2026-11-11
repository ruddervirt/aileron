// SPDX-License-Identifier: GPL-3.0-only

package build

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRelayHTTPFiles(t *testing.T) {
	inline := v1alpha1.BuildFile{Name: "preseed.cfg", Inline: "answer file"}
	url := v1alpha1.BuildFile{Name: "remote.cfg", URL: "https://example.com/remote.cfg"}
	for _, tt := range []struct {
		name    string
		files   []v1alpha1.BuildFile
		refs    []v1alpha1.HttpDirectoryFileRef
		wantCM  bool
		wantErr bool
	}{
		{"inline", []v1alpha1.BuildFile{inline}, []v1alpha1.HttpDirectoryFileRef{{Name: inline.Name}}, true, false},
		{"URL only", []v1alpha1.BuildFile{url}, []v1alpha1.HttpDirectoryFileRef{{Name: url.Name}}, false, false},
		{"mixed", []v1alpha1.BuildFile{inline, url}, []v1alpha1.HttpDirectoryFileRef{{Name: inline.Name}, {Name: url.Name}}, true, false},
		{"URL with unrelated inline file", []v1alpha1.BuildFile{inline, url}, []v1alpha1.HttpDirectoryFileRef{{Name: url.Name}}, false, false},
		{"missing", nil, []v1alpha1.HttpDirectoryFileRef{{Name: inline.Name}}, false, true},
		{"missing after valid", []v1alpha1.BuildFile{inline}, []v1alpha1.HttpDirectoryFileRef{{Name: inline.Name}, {Name: "missing"}}, false, true},
		{"empty content", []v1alpha1.BuildFile{{Name: inline.Name}}, []v1alpha1.HttpDirectoryFileRef{{Name: inline.Name}}, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			b := &v1alpha1.VirtualMachineBuild{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec:       v1alpha1.VirtualMachineBuildSpec{Files: tt.files, HttpDirectory: &v1alpha1.HttpDirectory{Files: tt.refs}},
				Status:     v1alpha1.VirtualMachineBuildStatus{BuildID: "vm-test", BuildNamespace: "default"},
			}
			cl := fake.NewClientBuilder().WithScheme(relayScheme(t)).Build()
			mgr := &RelayPodManager{Client: cl}
			if err := EnsureFilesConfigMap(ctx, cl, b); err != nil {
				t.Fatal(err)
			}
			err := mgr.EnsureRelayPod(ctx, b)
			pods := &corev1.PodList{}
			if listErr := cl.List(ctx, pods); listErr != nil {
				t.Fatal(listErr)
			}
			if tt.wantErr {
				if err == nil || len(pods.Items) != 0 {
					t.Fatalf("EnsureRelayPod = %v, pods = %d; want error and no pod", err, len(pods.Items))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			pod := &pods.Items[0]
			hasCM := false
			for _, volume := range pod.Spec.Volumes {
				if volume.ConfigMap != nil {
					hasCM = true
					if err := cl.Get(ctx, types.NamespacedName{Name: volume.ConfigMap.Name, Namespace: pod.Namespace}, &corev1.ConfigMap{}); err != nil {
						t.Fatalf("required ConfigMap missing: %v", err)
					}
				}
			}
			if hasCM != tt.wantCM {
				t.Fatalf("ConfigMap volume = %v, want %v", hasCM, tt.wantCM)
			}
			init := pod.Spec.InitContainers[0]
			hasCMMount := false
			for _, mount := range init.VolumeMounts {
				if mount.Name == "files-cm" {
					hasCMMount = true
				}
			}
			if hasCMMount != tt.wantCM {
				t.Fatalf("ConfigMap mount = %v, want %v", hasCMMount, tt.wantCM)
			}
			for _, ref := range tt.refs {
				f, _ := ResolveFile(b, ref.Name)
				want := "cp /files-cm/" + f.Name + " /http/" + f.Name
				if f.URL != "" {
					want = "wget -q -O /http/" + f.Name + " '" + f.URL + "'"
				}
				if !strings.Contains(init.Command[2], want) {
					t.Fatalf("script %q missing %q", init.Command[2], want)
				}
			}
		})
	}
}

func TestRelayWaitingDetails(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    corev1.PodStatus
		missingCM bool
		want      string
		ready     bool
	}{
		{name: "missing ConfigMap", missingCM: true, want: `ConfigMap "vm-test-files" not found`},
		{name: "init download failure", status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "http-files", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}}}}, want: "http-files: Error (exit 1)"},
		{name: "image pull", status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "relay", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull failed"}}}}}, want: "relay: ImagePullBackOff: pull failed"},
		{name: "unschedulable", status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "insufficient memory"}}}, want: "Unschedulable: insufficient memory"},
		{name: "running but not ready", status: corev1.PodStatus{Phase: corev1.PodRunning}, want: "Waiting for relay pod"},
		{name: "ready", status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}, ready: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := &v1alpha1.VirtualMachineBuild{Status: v1alpha1.VirtualMachineBuildStatus{BuildID: "vm-test", BuildNamespace: "default", Message: "Waiting for relay pod vm-test-relay"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vm-test-relay", Namespace: "default"}, Status: tt.status}
			if tt.missingCM {
				pod.Spec.Volumes = []corev1.Volume{{Name: "files-cm", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "vm-test-files"}}}}}
			}
			cl := fake.NewClientBuilder().WithScheme(relayScheme(t)).WithObjects(pod).Build()
			ready, err := (&RelayPodManager{Client: cl}).IsRelayReady(context.Background(), b)
			if err != nil || ready != tt.ready {
				t.Fatalf("IsRelayReady = %v, %v; want %v", ready, err, tt.ready)
			}
			if tt.ready && b.Status.Message != "" {
				t.Fatalf("stale wait message: %s", b.Status.Message)
			}
			if !tt.ready && !strings.Contains(b.Status.Message, tt.want) {
				t.Fatalf("message = %q, want %q", b.Status.Message, tt.want)
			}
		})
	}
}
