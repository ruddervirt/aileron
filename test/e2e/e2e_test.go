package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	dynClient dynamic.Interface

	buildGVR = schema.GroupVersionResource{
		Group: "ruddervirt.io", Version: "v1alpha1", Resource: "virtualmachinebuilds",
	}
	cloneGVR = schema.GroupVersionResource{
		Group: "ruddervirt.io", Version: "v1alpha1", Resource: "virtualmachineclones",
	}
)

func TestMain(m *testing.M) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic("failed to load kubeconfig: " + err.Error())
	}
	dynClient, err = dynamic.NewForConfig(config)
	if err != nil {
		panic("failed to create client: " + err.Error())
	}
	os.Exit(m.Run())
}

func TestBuildCRDInstalled(t *testing.T) {
	_, err := dynClient.Resource(buildGVR).Namespace("default").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("VirtualMachineBuild CRD not available: %v", err)
	}
}

func TestCloneCRDInstalled(t *testing.T) {
	_, err := dynClient.Resource(cloneGVR).Namespace("default").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("VirtualMachineClone CRD not available: %v", err)
	}
}

func TestCreateAndDeleteBuild(t *testing.T) {
	ctx := context.TODO()
	build := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "ruddervirt.io/v1alpha1",
			"kind":       "VirtualMachineBuild",
			"metadata": map[string]any{
				"name":      "e2e-test-build",
				"namespace": "default",
			},
			"spec": map[string]any{
				"vms": []any{
					map[string]any{
						"name": "builder",
						"source": map[string]any{
							"url": "https://example.com/disk.qcow2",
						},
						"resources": map[string]any{
							"cpu":    "1",
							"memory": "1Gi",
						},
					},
				},
			},
		},
	}

	created, err := dynClient.Resource(buildGVR).Namespace("default").Create(ctx, build, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create VirtualMachineBuild: %v", err)
	}
	t.Cleanup(func() {
		_ = dynClient.Resource(buildGVR).Namespace("default").Delete(ctx, "e2e-test-build", metav1.DeleteOptions{})
	})

	if created.GetName() != "e2e-test-build" {
		t.Errorf("expected name e2e-test-build, got %s", created.GetName())
	}

	got, err := dynClient.Resource(buildGVR).Namespace("default").Get(ctx, "e2e-test-build", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get VirtualMachineBuild: %v", err)
	}

	spec, ok := got.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec not found in created resource")
	}
	vms, ok := spec["vms"].([]any)
	if !ok || len(vms) != 1 {
		t.Fatalf("expected 1 VM in spec, got %v", vms)
	}
}

func TestCreateAndDeleteClone(t *testing.T) {
	ctx := context.TODO()
	clone := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "ruddervirt.io/v1alpha1",
			"kind":       "VirtualMachineClone",
			"metadata": map[string]any{
				"name":      "e2e-test-clone",
				"namespace": "default",
			},
			"spec": map[string]any{
				"templateName": "nonexistent-template",
			},
		},
	}

	created, err := dynClient.Resource(cloneGVR).Namespace("default").Create(ctx, clone, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create VirtualMachineClone: %v", err)
	}
	t.Cleanup(func() {
		_ = dynClient.Resource(cloneGVR).Namespace("default").Delete(ctx, "e2e-test-clone", metav1.DeleteOptions{})
	})

	if created.GetName() != "e2e-test-clone" {
		t.Errorf("expected name e2e-test-clone, got %s", created.GetName())
	}
}
