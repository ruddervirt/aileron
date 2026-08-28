// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// newTestServerAndClient is like newTestServer but also returns the
// underlying fake client, for tests that need to inspect state a handler
// mutated (e.g. a power-control action's effect on a VirtualMachine).
func newTestServerAndClient(t *testing.T, objs ...client.Object) (*httptest.Server, client.Client) {
	t.Helper()
	c := ctrlfake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	srv := NewServer(c, kubefake.NewClientset(), "ruddervirt-system", "ws://gw:7778",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return httptest.NewServer(srv.Handler()), c
}

// TestUIBuildsFragmentHasStableIDs checks the /ui/builds fragment carries the
// stable per-build and per-provisioners-section ids the frontend relies on
// for idiomorph to preserve open <details>/scroll/focus state across polls.
func TestUIBuildsFragmentHasStableIDs(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "test-simple", Namespace: "ruddervirt-system"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase:          "Succeeded",
			BuildID:        "vm-abc123",
			BuildNamespace: "vm-abc123",
			VMStatuses: []v1alpha1.VMBuildStatus{{
				Name:   "builder",
				Phase:  "Succeeded",
				VMName: "vm-abc123-builder",
				ProvisionerResults: []v1alpha1.ProvisionerResult{
					{Index: 0, Type: "shell", Name: "hello", Status: "Succeeded", Duration: &metav1.Duration{Duration: 3 * time.Second}},
				},
			}},
		},
	}
	ts := newTestServer(t, build, runningVMI("vm-abc123", "vm-abc123-builder"))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/builds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)

	for _, want := range []string{
		`id="build-test-simple"`,
		`id="provs-test-simple"`,
		`hello (shell)`,
		`console: builder`,
		// a Succeeded build gets the inline clone mini-form.
		`hx-post="/ui/clones"`,
		`value="test-simple"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fragment missing %q\nfull body:\n%s", want, html)
		}
	}
}

// TestUIBuildsFragmentEmpty checks the "none" placeholder renders when there
// are no builds.
func TestUIBuildsFragmentEmpty(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/builds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<div class="empty">none</div>`) {
		t.Errorf("body = %q, want the empty placeholder", body)
	}
}

// TestUICreateBuildInvalidManifest checks a malformed manifest is rejected
// with a 400 and an inline error fragment, so the response-targets extension
// can route it to the form's status line.
func TestUICreateBuildInvalidManifest(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.PostForm(ts.URL+"/ui/builds", map[string][]string{
		"manifest": {"not: valid: yaml: ::"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "parsing manifest") {
		t.Errorf("body = %q, want a parsing-manifest error", body)
	}
}

// TestUICreateBuildSuccessRendersPanelAndStatus checks a valid submission
// renders the updated builds panel plus an out-of-band "created" status
// update for the form.
func TestUICreateBuildSuccessRendersPanelAndStatus(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	manifest := `apiVersion: ruddervirt.io/v1alpha1
kind: VirtualMachineBuild
metadata:
  name: ui-build
spec:
  vms:
    - name: builder
      source:
        url: "https://example.com/img.qcow2"
`
	resp, err := http.PostForm(ts.URL+"/ui/builds", map[string][]string{"manifest": {manifest}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, `id="build-ui-build"`) {
		t.Errorf("panel missing new build: %s", html)
	}
	if !strings.Contains(html, `id="build-status" class="form-status" hx-swap-oob="true"`) {
		t.Errorf("missing out-of-band status update: %s", html)
	}
	if !strings.Contains(html, "created ui-build") {
		t.Errorf("missing created confirmation: %s", html)
	}
}

// TestUIDeleteBuildRerendersPanel checks deleting a build renders the
// resulting (now-empty) panel rather than a bare 204.
func TestUIDeleteBuildRerendersPanel(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "test-simple", Namespace: "ruddervirt-system"},
	}
	ts := newTestServer(t, build)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/ui/builds/test-simple", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `<div class="empty">none</div>`) {
		t.Errorf("body = %q, want the empty placeholder after delete", body)
	}
}

// TestUIDeleteMissingBuildIsLenient checks deleting an already-gone build
// still renders 200 with the current panel (matching the old frontend's
// "ignore 404 on delete" behavior), rather than surfacing an error.
func TestUIDeleteMissingBuildIsLenient(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/ui/builds/nope", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestUICreateCloneFromForm checks the inline clone-from-build mini-form path
// (name + templateName fields, not a YAML manifest).
func TestUICreateCloneFromForm(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.PostForm(ts.URL+"/ui/clones", map[string][]string{
		"name":         {"my-clone"},
		"templateName": {"test-simple"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	if !strings.Contains(html, `id="clone-my-clone"`) {
		t.Errorf("panel missing new clone: %s", html)
	}
	if !strings.Contains(html, "template: test-simple") {
		t.Errorf("missing template reference: %s", html)
	}
	if !strings.Contains(html, `id="clone-status-test-simple"`) {
		t.Errorf("missing out-of-band clone status target: %s", html)
	}
}

// TestUICreateCloneMissingFields rejects a clone submission missing required
// fields with a 400.
func TestUICreateCloneMissingFields(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.PostForm(ts.URL+"/ui/clones", map[string][]string{"name": {"my-clone"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestUIGradesFragmentRendersQueuePosition checks a queued grade VM's
// queuePosition pointer is dereferenced correctly in the rendered fragment
// (a common text/template pointer-printing pitfall).
func TestUIGradesFragmentRendersQueuePosition(t *testing.T) {
	pos := int32(3)
	grade := &v1alpha1.GradeRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "grade-sample", Namespace: "ruddervirt-system"},
		Spec:       v1alpha1.GradeRequestSpec{Namespace: "ns-example"},
		Status: v1alpha1.GradeRequestStatus{
			Phase: v1alpha1.GradeRequestPhaseRunning,
			VMStatuses: []v1alpha1.GradeVMStatus{
				{Name: "clone-simple-builder", Phase: "Queued", QueuePosition: &pos},
			},
		},
	}
	ts := newTestServer(t, grade)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/grades")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "queued #3") {
		t.Errorf("body = %q, want it to render the dereferenced queue position", body)
	}
	if strings.Contains(string(body), "0x") {
		t.Errorf("body = %q, contains a raw pointer address instead of a dereferenced value", body)
	}
}

// TestUIBuildsFragmentHidesConsoleWhenNotRunning checks that a build's
// console link is omitted when its VM has no running VirtualMachineInstance
// (e.g. the VM hasn't booted yet, or was stopped) — a console link to a
// stopped VM has nothing to connect to.
func TestUIBuildsFragmentHidesConsoleWhenNotRunning(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "test-simple", Namespace: "ruddervirt-system"},
		Status: v1alpha1.VirtualMachineBuildStatus{
			Phase:          "Building",
			BuildID:        "vm-abc123",
			BuildNamespace: "vm-abc123",
			VMStatuses:     []v1alpha1.VMBuildStatus{{Name: "builder", VMName: "vm-abc123-builder"}},
		},
	}
	ts := newTestServer(t, build) // no VirtualMachineInstance fixture: not running
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/builds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "console:") {
		t.Errorf("body = %q, want no console link for a non-running VM", body)
	}
}

// TestUIClonesPanelShowsPowerControls checks the clones panel renders a
// console link + stop button for a running clone VM, and a start button (no
// console link) for a stopped one.
func TestUIClonesPanelShowsPowerControls(t *testing.T) {
	clone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "test-clone", Namespace: "ruddervirt-system"},
		Spec:       v1alpha1.VirtualMachineCloneSpec{TemplateName: "module"},
		Status: v1alpha1.VirtualMachineCloneStatus{
			Phase:          "Ready",
			CloneNamespace: "ns-abc123",
			VMStatuses: []v1alpha1.ClonedVMStatus{
				{Name: "run-vm", Ready: true},
				{Name: "stop-vm", Ready: true},
			},
		},
	}
	ts := newTestServer(t, clone, runningVMI("ns-abc123", "run-vm"))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/clones")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	for _, want := range []string{
		`href="/console.html?ns=ns-abc123&amp;vmi=run-vm&amp;name=run-vm"`,
		`hx-post="/ui/clones/test-clone/vms/run-vm/stop?page=1"`,
		`hx-post="/ui/clones/test-clone/vms/stop-vm/start?page=1"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fragment missing %q\nfull body:\n%s", want, html)
		}
	}
	if strings.Contains(html, "vmi=stop-vm") {
		t.Errorf("fragment has a console link for the stopped VM: %s", html)
	}
}

// TestUIStartCloneVM checks the start action patches the target VM's
// spec.runStrategy to Always.
func TestUIStartCloneVM(t *testing.T) {
	clone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "test-clone", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineCloneStatus{CloneNamespace: "ns-abc123"},
	}
	ts, c := newTestServerAndClient(t, clone, stoppedVM("ns-abc123", "run-vm"))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/ui/clones/test-clone/vms/run-vm/start", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}

	var vm unstructured.Unstructured
	vm.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"})
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns-abc123", Name: "run-vm"}, &vm); err != nil {
		t.Fatal(err)
	}
	strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	if strategy != "Always" {
		t.Errorf("runStrategy = %q, want Always", strategy)
	}
}

// TestUIBuildsSortedAndPaginated checks builds are listed newest-first and
// split across pages of uiPageSize.
func TestUIBuildsSortedAndPaginated(t *testing.T) {
	objs := make([]client.Object, 0, uiPageSize+2)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= uiPageSize+2; i++ {
		objs = append(objs, &v1alpha1.VirtualMachineBuild{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmtBuildName(i),
				Namespace:         "ruddervirt-system",
				CreationTimestamp: metav1.NewTime(base.Add(time.Duration(i) * time.Minute)),
			},
		})
	}
	ts := newTestServer(t, objs...)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/builds")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Newest (highest i, latest timestamp) first.
	newest := strings.Index(html, `id="build-`+fmtBuildName(uiPageSize+2)+`"`)
	oldestOnPage := strings.Index(html, `id="build-`+fmtBuildName(3)+`"`)
	if newest == -1 || oldestOnPage == -1 || newest > oldestOnPage {
		t.Fatalf("expected newest build before oldest-shown build in page 1; body:\n%s", html)
	}
	if strings.Contains(html, `id="build-`+fmtBuildName(1)+`"`) {
		t.Errorf("page 1 should not contain the oldest build (past uiPageSize): %s", html)
	}
	if !strings.Contains(html, "page 1 / 2") {
		t.Errorf("missing pager text 'page 1 / 2': %s", html)
	}

	resp2, err := http.Get(ts.URL + "/ui/builds?page=2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	body2, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body2), `id="build-`+fmtBuildName(1)+`"`) {
		t.Errorf("page 2 should contain the oldest build: %s", body2)
	}
}

func fmtBuildName(i int) string {
	return fmt.Sprintf("build-%02d", i)
}
