// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

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
	ts := newTestServer(t, build)
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
