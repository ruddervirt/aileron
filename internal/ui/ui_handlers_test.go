// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
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
			Phase:          testPhaseSucceeded,
			BuildID:        "vm-abc123",
			BuildNamespace: "vm-abc123",
			VMStatuses: []v1alpha1.VMBuildStatus{{
				Name:   "builder",
				Phase:  testPhaseSucceeded,
				VMName: "vm-abc123-builder",
				ProvisionerResults: []v1alpha1.ProvisionerResult{
					{Index: 0, Type: "shell", Name: "hello", Status: testPhaseSucceeded, Duration: &metav1.Duration{Duration: 3 * time.Second}},
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
		// a Succeeded build gets the one-click clone button.
		`hx-post="/ui/clones"`,
		`hx-vals='{"templateName": "test-simple"}'`,
		// and its own spec, viewable inline.
		`id="spec-test-simple"`,
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

// TestUICreateCloneMissingFields rejects a clone submission missing
// templateName with a 400.
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

// TestUICreateCloneGeneratesName checks the one-click clone path (just
// templateName, matching the build card's button — no name field, no
// confirmation) gets a unique, DNS-1123-safe generated name.
func TestUICreateCloneGeneratesName(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.PostForm(ts.URL+"/ui/clones", map[string][]string{"templateName": {"test-simple"}})
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
	if !generatedCloneNameRE.MatchString(html) {
		t.Errorf("body missing a generated ns-<cuid2> clone id: %s", html)
	}
}

var generatedCloneNameRE = regexp.MustCompile(`id="clone-ns-[a-z][a-z0-9]{23}"`)

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
func TestUIClonesPanelShowsPhaseAndConsoleLinks(t *testing.T) {
	build := &v1alpha1.VirtualMachineBuild{
		ObjectMeta: metav1.ObjectMeta{Name: "module", Namespace: "ruddervirt-system"},
		Spec: v1alpha1.VirtualMachineBuildSpec{
			VMs: []v1alpha1.BuildVM{
				{Name: "run-vm", Communicator: v1alpha1.BuildCommunicator{SSHUsername: "debian"}},
				{Name: "stop-vm", Communicator: v1alpha1.BuildCommunicator{SSHUsername: "debian"}},
			},
		},
	}
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
	ts := newTestServer(t, build, clone, runningVMI("ns-abc123", "run-vm"))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/ui/clones")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// No start/stop buttons in the clone card any more — those moved to the
	// console page — but a console link (tagged with clone+vm) is always
	// present so an operator can reach the console page to start a stopped
	// VM, and each VM shows its live phase.
	for _, want := range []string{
		`href="/console.html?ns=ns-abc123&amp;vmi=run-vm&amp;name=run-vm&amp;clone=test-clone&amp;vm=run-vm"`,
		`href="/console.html?ns=ns-abc123&amp;vmi=stop-vm&amp;name=stop-vm&amp;clone=test-clone&amp;vm=stop-vm"`,
		`<span class="badge ok">Running</span>`,
		`<span class="badge warn">Stopped</span>`,
		"name: run-vm",
		"name: stop-vm",
		"user: debian",
		"namespace: ns-abc123",
		"commands: []",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fragment missing %q\nfull body:\n%s", want, html)
		}
	}
	for _, want := range []string{
		`hx-post="/ui/clones/test-clone/vms/run-vm/stop?target=vm-power-test-clone-run-vm"`,
		`hx-post="/ui/clones/test-clone/vms/stop-vm/start?target=vm-power-test-clone-stop-vm"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fragment missing %q (start/stop buttons belong on the clone card too, not just the console page)\nfull body:\n%s", want, html)
		}
	}
}

// TestUIStartCloneVM checks the start action patches the target VM's
// spec.runStrategy to Always and renders the power widget.
func TestUIStartCloneVM(t *testing.T) {
	clone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "test-clone", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineCloneStatus{CloneNamespace: "ns-abc123"},
	}
	ts, c := newTestServerAndClient(t, clone, stoppedVM("run-vm"))
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
	body, _ := io.ReadAll(resp.Body)
	// The VM is now desired-running but has no VMI yet: "Starting", not
	// "Running" — the whole point of the phase split.
	if !strings.Contains(string(body), "Starting") {
		t.Errorf("body = %q, want the Starting phase immediately after start", body)
	}

	var vm unstructured.Unstructured
	vm.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"})
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns-abc123", Name: "run-vm"}, &vm); err != nil {
		t.Fatal(err)
	}
	strategy, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	if strategy != testRunStrategyAlways {
		t.Errorf("runStrategy = %q, want Always", strategy)
	}
}

// TestUICloneVMPowerPhases checks the power-status endpoint reports all four
// phases correctly, including the transitional Starting/Stopping states a
// plain VMI-running check would miss.
func TestUICloneVMPowerPhases(t *testing.T) {
	clone := &v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: "test-clone", Namespace: "ruddervirt-system"},
		Status:     v1alpha1.VirtualMachineCloneStatus{CloneNamespace: "ns-abc123"},
	}

	runningVM := stoppedVM("running-vm")
	_ = unstructured.SetNestedField(runningVM.Object, testRunStrategyAlways, "spec", "runStrategy")

	stoppingVM := stoppedVM("stopping-vm") // runStrategy Halted, but VMI still tearing down

	ts := newTestServer(t, clone,
		runningVM, runningVMI("ns-abc123", "running-vm"),
		stoppedVM("stopped-vm"), // Halted, no VMI
		stoppingVM, vmiWithPhase("ns-abc123", "stopping-vm", "Scheduled"),
	)
	defer ts.Close()

	for vm, want := range map[string]string{
		"running-vm":  testPhaseRunning,
		"stopped-vm":  "Stopped",
		"stopping-vm": "Stopping",
	} {
		resp, err := http.Get(ts.URL + "/ui/clones/test-clone/vms/" + vm + "/power")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Errorf("power(%s) = %q, want it to contain %q", vm, body, want)
		}
	}
}

// TestUIConsolePageShowsPowerWidgetOnlyForClones checks the console page
// includes the power widget div when reached via a clone VM's console link
// (clone+vm query params), and omits it for a plain build console link.
func TestUIConsolePageShowsPowerWidgetOnlyForClones(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/console.html?ns=ns-1&vmi=vm-1&name=vm-1&clone=my-clone&vm=vm-1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), `hx-get="/ui/clones/my-clone/vms/vm-1/power"`) {
		t.Errorf("clone console page missing power widget: %s", body)
	}

	resp2, err := http.Get(ts.URL + "/console.html?ns=ns-1&vmi=vm-1&name=vm-1")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if strings.Contains(string(body2), `id="power"`) {
		t.Errorf("build console page should have no power widget: %s", body2)
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
	if !strings.Contains(html, `id="builds-pager" class="pager-inline" hx-swap-oob="true">`) || !strings.Contains(html, `<span class="page-info">1/2</span>`) {
		t.Errorf("missing out-of-band pager showing page 1/2: %s", html)
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
