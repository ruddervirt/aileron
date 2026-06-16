package ui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestServer(t *testing.T, objs ...client.Object) *httptest.Server {
	t.Helper()
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	srv := NewServer(c, fake.NewClientset(), "ruddervirt-system", "ws://gw:7778",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return httptest.NewServer(srv.Handler())
}

// TestListBuildsProjectsConsoles checks the build list projection derives a
// console target from status (buildNamespace + vmStatuses[].vmName).
func TestListBuildsProjectsConsoles(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "test-simple", Namespace: "ruddervirt-system"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase:          "Building",
			BuildID:        "vm-abc123",
			BuildNamespace: "vm-abc123",
			VMStatuses: []v1alpha1.VMBuildStatus{
				{Name: "builder", VMName: "vm-abc123-builder"},
			},
		},
	}
	ts := newTestServer(t, build)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/builds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var views []buildView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d builds, want 1", len(views))
	}
	if views[0].Phase != "Building" {
		t.Errorf("phase = %q, want Building", views[0].Phase)
	}
	if len(views[0].Consoles) != 1 {
		t.Fatalf("got %d consoles, want 1", len(views[0].Consoles))
	}
	c := views[0].Consoles[0]
	if c.Namespace != "vm-abc123" || c.VMI != "vm-abc123-builder" || c.VMName != "builder" {
		t.Errorf("console = %+v, want ns=vm-abc123 vmi=vm-abc123-builder name=builder", c)
	}
}

// TestBuildProvisionerStatus checks per-VM provisioner results are projected
// into the build view.
func TestBuildProvisionerStatus(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "test-simple", Namespace: "ruddervirt-system"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase:          "Building",
			BuildID:        "vm-abc123",
			BuildNamespace: "ruddervirt-system",
			VMStatuses: []v1alpha1.VMBuildStatus{{
				Name:   "builder",
				Phase:  "Provisioning",
				VMName: "vm-abc123-builder",
				ProvisionerResults: []v1alpha1.ProvisionerResult{
					{Index: 0, Type: "shell", Name: "hello", Status: "Succeeded", Duration: &metav1.Duration{Duration: 3 * time.Second}},
					{Index: 1, Type: "shell", Name: "fail", Status: "Failed", Message: "exit code 1"},
				},
			}},
		},
	}
	ts := newTestServer(t, build)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/builds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	var views []buildView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || len(views[0].VMs) != 1 {
		t.Fatalf("want 1 build with 1 vm, got %+v", views)
	}
	provs := views[0].VMs[0].Provisioners
	if len(provs) != 2 {
		t.Fatalf("got %d provisioners, want 2", len(provs))
	}
	if provs[0].Name != "hello" || provs[0].Status != "Succeeded" || provs[0].Duration != "3s" {
		t.Errorf("prov[0] = %+v, want hello/Succeeded/3s", provs[0])
	}
	if provs[1].Status != "Failed" || provs[1].Message != "exit code 1" {
		t.Errorf("prov[1] = %+v, want Failed with message", provs[1])
	}
}

// TestBuildLogs streams the coordinator pod logs for a build's VM.
func TestBuildLogs(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "test-simple", Namespace: "ruddervirt-system"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			BuildID:        "vm-abc123",
			BuildNamespace: "ruddervirt-system",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vm-abc123-coordinator-builder-xyz",
			Namespace: "ruddervirt-system",
			Labels:    map[string]string{"batch.kubernetes.io/job-name": "vm-abc123-coordinator-builder"},
		},
	}
	ts := newTestServer(t, build, pod)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/builds/test-simple/logs?vm=builder")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	// The fake clientset returns "fake logs" for any GetLogs stream.
	if !strings.Contains(string(body), "fake logs") {
		t.Errorf("log body = %q, want it to contain the streamed logs", string(body))
	}
}

// TestBuildLogsRequiresVM rejects a logs request without ?vm=.
func TestBuildLogsRequiresVM(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/builds/test-simple/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestCreateBuildFromYAML verifies a YAML manifest body is decoded and created,
// defaulting the namespace to the server's namespace.
func TestCreateBuildFromYAML(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	manifest := `apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: yaml-build
spec:
  vms:
    - name: builder
      source:
        url: "https://example.com/img.qcow2"
`
	resp, err := http.Post(ts.URL+"/api/builds", "application/yaml", strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, body)
	}

	var view buildView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "yaml-build" {
		t.Errorf("name = %q, want yaml-build", view.Name)
	}
	if view.Namespace != "ruddervirt-system" {
		t.Errorf("namespace = %q, want ruddervirt-system (default)", view.Namespace)
	}
}

// TestCreateBuildEmptyBody rejects an empty manifest with 400.
func TestCreateBuildEmptyBody(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/builds", "application/yaml", strings.NewReader("  \n"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestListGradesProjectsVMs checks the grade list projection surfaces phase,
// target namespace, and per-VM statuses.
func TestListGradesProjectsVMs(t *testing.T) {
	grade := &v1alpha1.GradeRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "grade-sample", Namespace: "ruddervirt-system"},
		Spec:       v1alpha1.GradeRequestSpec{Namespace: "ns-example"},
		Status: v1alpha1.GradeRequestStatus{
			Phase: v1alpha1.GradeRequestPhaseRunning,
			VMStatuses: []v1alpha1.GradeVMStatus{
				{Name: "clone-simple-builder", Phase: v1alpha1.GradeRequestPhaseReady, JobName: "grade-sample-0"},
			},
		},
	}
	ts := newTestServer(t, grade)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/grades")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var views []gradeView
	if err := json.NewDecoder(resp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d grades, want 1", len(views))
	}
	g := views[0]
	if g.Phase != "Running" || g.TargetNamespace != "ns-example" {
		t.Errorf("grade = %+v, want phase=Running targetNamespace=ns-example", g)
	}
	if len(g.VMs) != 1 || g.VMs[0].Name != "clone-simple-builder" || g.VMs[0].Phase != "Ready" {
		t.Errorf("grade VMs = %+v, want one Ready clone-simple-builder", g.VMs)
	}
}

// TestCreateGradeFromYAML verifies a GradeRequest YAML body is decoded and
// created, defaulting the namespace to the server's namespace.
func TestCreateGradeFromYAML(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	manifest := `apiVersion: ruddervirt.io/v1alpha1
kind: GradeRequest
metadata:
  name: yaml-grade
spec:
  namespace: ns-example
  vms:
    - name: vm-1
      user: debian
      password: ""
      commands:
        - uname -a
`
	resp, err := http.Post(ts.URL+"/api/grades", "application/yaml", strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, body)
	}

	var view gradeView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Name != "yaml-grade" {
		t.Errorf("name = %q, want yaml-grade", view.Name)
	}
	if view.Namespace != "ruddervirt-system" {
		t.Errorf("namespace = %q, want ruddervirt-system (default)", view.Namespace)
	}
	if view.TargetNamespace != "ns-example" {
		t.Errorf("targetNamespace = %q, want ns-example", view.TargetNamespace)
	}
}

// TestDeleteMissingBuild returns 404.
func TestDeleteMissingBuild(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/builds/nope", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
