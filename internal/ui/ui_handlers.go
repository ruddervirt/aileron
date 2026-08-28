// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

// uiPageSize is the number of items shown per page in the builds/clones
// panels.
const uiPageSize = 10

// pageParam reads the "page" parameter (query string for GET, form body for
// POST/PUT/PATCH) and clamps it to >= 1. DELETE requests carry it in the URL
// query string instead (see the hx-delete URLs in fragments.html.tmpl),
// since net/http's form parsing does not read DELETE bodies.
func pageParam(r *http.Request) int {
	raw := r.URL.Query().Get("page")
	if raw == "" {
		raw = r.FormValue("page")
	}
	p, err := strconv.Atoi(raw)
	if err != nil || p < 1 {
		return 1
	}
	return p
}

// buildsPanelView is the data passed to the builds-panel template: one page
// of items plus pagination info.
type buildsPanelView struct {
	Items      []buildItemView
	Page       int
	TotalPages int
}

// buildItemView is a build carrying the page it was rendered on, so its
// delete button's URL can round-trip back to the same page.
type buildItemView struct {
	buildView
	Page int
}

// paginateBuilds pages views and, only for the items on the returned page,
// filters each build's consoles down to VMs that are actually running (a
// console link to a stopped VM has nothing to connect to).
func (s *Server) paginateBuilds(ctx context.Context, views []buildView, page int) buildsPanelView {
	pageViews, page, total := paginate(views, page)
	items := make([]buildItemView, 0, len(pageViews))
	for _, v := range pageViews {
		v.Consoles = s.runningConsoles(ctx, v.Consoles)
		items = append(items, buildItemView{buildView: v, Page: page})
	}
	return buildsPanelView{Items: items, Page: page, TotalPages: total}
}

// clonesPanelView and cloneItemView mirror buildsPanelView/buildItemView for
// clones.
type clonesPanelView struct {
	Items      []cloneItemView
	Page       int
	TotalPages int
}

type cloneItemView struct {
	cloneView
	Page int
	// VMs carries live power state per VM (unlike buildItemView, clone VMs
	// are always listed — running or not — since the console page, where
	// power controls now live, needs a target VM even when it's stopped).
	VMs []cloneVMView
	// GradeManifest is a pre-filled GradeRequest YAML manifest naming this
	// clone's VMs, for the "grade" mini-form. Empty (and the form hidden) if
	// the clone has no VMs yet.
	GradeManifest string
}

// cloneVMView is one VM within a clone, enriched with its live power phase
// for the /ui clones panel (a status badge; the console link always renders
// since power controls, and thus the ability to start a stopped VM, now live
// on the console page itself).
type cloneVMView struct {
	VMName    string
	Namespace string
	VMI       string
	Phase     string
}

// paginateClones pages views and, only for the items on the returned page,
// looks up each clone VM's live power phase and builds its grade-form
// manifest.
func (s *Server) paginateClones(ctx context.Context, views []cloneView, page int) clonesPanelView {
	pageViews, page, total := paginate(views, page)
	items := make([]cloneItemView, 0, len(pageViews))
	for _, v := range pageViews {
		vms := make([]cloneVMView, 0, len(v.Consoles))
		vmNames := make([]string, 0, len(v.Consoles))
		for _, c := range v.Consoles {
			vms = append(vms, cloneVMView{
				VMName:    c.VMName,
				Namespace: c.Namespace,
				VMI:       c.VMI,
				Phase:     string(s.vmPowerPhase(ctx, c.Namespace, c.VMI)),
			})
			vmNames = append(vmNames, c.VMName)
		}
		item := cloneItemView{cloneView: v, Page: page, VMs: vms}
		if len(vmNames) > 0 {
			item.GradeManifest = s.cloneGradeManifest(ctx, v, v.Consoles[0].Namespace, vmNames)
		}
		items = append(items, item)
	}
	return clonesPanelView{Items: items, Page: page, TotalPages: total}
}

// cloneGradeManifest builds a pre-filled GradeRequest YAML manifest for a
// clone's VMs: names and (best-effort) a shared SSH username inherited from
// the source build, target namespace (the clone's own ephemeral VM
// namespace, not the VirtualMachineClone CR's namespace), and empty commands
// for the operator to fill in.
func (s *Server) cloneGradeManifest(ctx context.Context, c cloneView, vmNamespace string, vmNames []string) string {
	user := s.defaultSSHUsername(ctx, c.Namespace, c.TemplateName)
	var b strings.Builder
	b.WriteString("apiVersion: ruddervirt.io/v1alpha1\n")
	b.WriteString("kind: GradeRequest\n")
	b.WriteString("metadata:\n")
	fmt.Fprintf(&b, "  name: %s-grade\n", c.Name)
	b.WriteString("spec:\n")
	fmt.Fprintf(&b, "  namespace: %s\n", vmNamespace)
	b.WriteString("  vms:\n")
	for _, name := range vmNames {
		fmt.Fprintf(&b, "    - name: %s\n", name)
		fmt.Fprintf(&b, "      user: %s\n", user)
		b.WriteString("      password: \"\"\n")
		b.WriteString("      commands: []\n")
	}
	return b.String()
}

// defaultSSHUsername returns the source build's communicator SSH username,
// if every one of its VMs shares the same non-empty value — otherwise "",
// leaving the field for the operator to fill in rather than guessing wrong.
func (s *Server) defaultSSHUsername(ctx context.Context, namespace, templateName string) string {
	if templateName == "" {
		return ""
	}
	var build v1alpha1.VirtualMachineBuild
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: templateName}, &build); err != nil {
		return ""
	}
	user := ""
	for _, vm := range build.Spec.VMs {
		u := vm.Communicator.SSHUsername
		if u == "" {
			continue
		}
		if user == "" {
			user = u
		} else if user != u {
			return ""
		}
	}
	return user
}

// paginate slices items into the requested page (1-indexed, clamped to the
// valid range) of size uiPageSize, returning that page's items alongside the
// clamped page number and the total page count (always >= 1).
func paginate[T any](items []T, page int) (pageItems []T, currentPage, totalPages int) {
	totalPages = max((len(items)+uiPageSize-1)/uiPageSize, 1)
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * uiPageSize
	end := min(start+uiPageSize, len(items))
	if start > len(items) {
		start = len(items)
	}
	return items[start:end], page, totalPages
}

// classifyCreateErr maps a Create error to an HTTP status and message,
// shared by the JSON (/api) and HTML (/ui) handlers.
func classifyCreateErr(err error, kind string) (status int, msg string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict, kind + " already exists"
	case apierrors.IsBadRequest(err) || apierrors.IsInvalid(err):
		return http.StatusUnprocessableEntity, "invalid " + kind + " manifest"
	default:
		return http.StatusInternalServerError, "creating " + kind
	}
}

// formManifest reads and validates the "manifest" field of a POSTed form.
func formManifest(r *http.Request) (string, error) {
	if err := r.ParseForm(); err != nil {
		return "", err
	}
	manifest := strings.TrimSpace(r.FormValue("manifest"))
	if manifest == "" {
		return "", errors.New("empty manifest")
	}
	return manifest, nil
}

// --- builds ---

func (s *Server) uiListBuilds(w http.ResponseWriter, r *http.Request) {
	views, err := s.fetchBuilds(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing builds: "+err.Error())
		return
	}
	s.renderFragment(w, "builds-panel", s.paginateBuilds(r.Context(), views, pageParam(r)))
}

func (s *Server) uiCreateBuild(w http.ResponseWriter, r *http.Request) {
	manifest, err := formManifest(r)
	if err != nil {
		s.renderErrorMessage(w, http.StatusBadRequest, err.Error())
		return
	}
	var b v1alpha1.VirtualMachineBuild
	if err := yaml.Unmarshal([]byte(manifest), &b); err != nil {
		s.renderErrorMessage(w, http.StatusBadRequest, "parsing manifest: "+err.Error())
		return
	}
	if b.Namespace == "" {
		b.Namespace = s.namespace
	}
	if err := s.client.Create(r.Context(), &b); err != nil {
		status, msg := classifyCreateErr(err, "build")
		if status != http.StatusConflict {
			msg += ": " + err.Error()
		}
		s.renderErrorMessage(w, status, msg)
		return
	}
	// A newly created build is always the most recent (sorted first), so it
	// always lands on page 1.
	s.renderBuildsPanel(w, r, 1, &oobStatus{ID: "build-status", Msg: "created " + b.Name})
}

func (s *Server) uiDeleteBuild(w http.ResponseWriter, r *http.Request) {
	b := v1alpha1.VirtualMachineBuild{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &b); err != nil && !apierrors.IsNotFound(err) {
		s.renderErrorMessage(w, http.StatusInternalServerError, "deleting build: "+err.Error())
		return
	}
	s.renderBuildsPanel(w, r, pageParam(r), nil)
}

func (s *Server) renderBuildsPanel(w http.ResponseWriter, r *http.Request, page int, oob *oobStatus) {
	views, err := s.fetchBuilds(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing builds: "+err.Error())
		return
	}
	s.renderPanel(w, "builds-panel", s.paginateBuilds(r.Context(), views, page), oob)
}

// --- clones ---

func (s *Server) uiListClones(w http.ResponseWriter, r *http.Request) {
	views, err := s.fetchClones(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing clones: "+err.Error())
		return
	}
	s.renderFragment(w, "clones-panel", s.paginateClones(r.Context(), views, pageParam(r)))
}

// uiCreateClone creates a VirtualMachineClone from the inline "clone" form on
// a succeeded build (name + templateName fields), not a YAML manifest.
func (s *Server) uiCreateClone(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderErrorMessage(w, http.StatusBadRequest, "parsing form: "+err.Error())
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	templateName := strings.TrimSpace(r.FormValue("templateName"))
	if name == "" || templateName == "" {
		s.renderErrorMessage(w, http.StatusBadRequest, "name and templateName are required")
		return
	}
	c := v1alpha1.VirtualMachineClone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace},
		Spec:       v1alpha1.VirtualMachineCloneSpec{TemplateName: templateName},
	}
	if err := s.client.Create(r.Context(), &c); err != nil {
		status, msg := classifyCreateErr(err, "clone")
		if status != http.StatusConflict {
			msg += ": " + err.Error()
		}
		s.renderErrorMessage(w, status, msg)
		return
	}
	s.renderClonesPanel(w, r, 1, &oobStatus{ID: "clone-status-" + templateName, Msg: "created " + name})
}

func (s *Server) uiDeleteClone(w http.ResponseWriter, r *http.Request) {
	c := v1alpha1.VirtualMachineClone{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &c); err != nil && !apierrors.IsNotFound(err) {
		s.renderErrorMessage(w, http.StatusInternalServerError, "deleting clone: "+err.Error())
		return
	}
	s.renderClonesPanel(w, r, pageParam(r), nil)
}

func (s *Server) renderClonesPanel(w http.ResponseWriter, r *http.Request, page int, oob *oobStatus) {
	views, err := s.fetchClones(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing clones: "+err.Error())
		return
	}
	s.renderPanel(w, "clones-panel", s.paginateClones(r.Context(), views, page), oob)
}

// clonePowerPhase looks up a clone VM's power phase, resolving the clone's
// ephemeral VM namespace from its status first.
func (s *Server) clonePowerPhase(ctx context.Context, cloneName, vmName string) (vmPowerPhase, error) {
	var c v1alpha1.VirtualMachineClone
	key := client.ObjectKey{Namespace: s.namespace, Name: cloneName}
	if err := s.client.Get(ctx, key, &c); err != nil {
		return "", err
	}
	if c.Status.CloneNamespace == "" {
		return vmPowerStopped, nil
	}
	return s.vmPowerPhase(ctx, c.Status.CloneNamespace, vmName), nil
}

// uiCloneVMPower renders the current power phase + control for one clone VM.
// The console page polls this to show live "Starting"/"Stopping" progress
// (KubeVirt takes up to roughly a minute to boot or tear down a VM) without
// the operator needing to guess whether anything is happening.
func (s *Server) uiCloneVMPower(w http.ResponseWriter, r *http.Request) {
	name, vmName := r.PathValue("name"), r.PathValue("vmName")
	phase, err := s.clonePowerPhase(r.Context(), name, vmName)
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "loading power state: "+err.Error())
		return
	}
	s.renderFragment(w, "power-widget", powerWidgetView{CloneName: name, VMName: vmName, Phase: string(phase), TargetID: "power"})
}

// uiSetClonePower switches one VM within a clone on or off, then renders the
// same "power" fragment uiCloneVMPower does. It's reachable from two places
// — a VM row on the clone card, and console.html's power panel — each with a
// different swap target, so the caller passes back which one via ?target=
// (defaulting to "power", the console page's fixed id).
func (s *Server) uiSetClonePower(w http.ResponseWriter, r *http.Request, running bool) {
	name, vmName := r.PathValue("name"), r.PathValue("vmName")
	target := r.URL.Query().Get("target")
	if target == "" {
		target = "power"
	}
	var c v1alpha1.VirtualMachineClone
	key := client.ObjectKey{Namespace: s.namespace, Name: name}
	if err := s.client.Get(r.Context(), key, &c); err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "loading clone: "+err.Error())
		return
	}
	if c.Status.CloneNamespace == "" {
		s.renderErrorMessage(w, http.StatusConflict, "clone has no VMs yet")
		return
	}
	if err := s.setVMRunning(r.Context(), c.Status.CloneNamespace, vmName, running); err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "setting power state: "+err.Error())
		return
	}
	phase := s.vmPowerPhase(r.Context(), c.Status.CloneNamespace, vmName)
	s.renderFragment(w, "power-widget", powerWidgetView{CloneName: name, VMName: vmName, Phase: string(phase), TargetID: target})
}

func (s *Server) uiStartCloneVM(w http.ResponseWriter, r *http.Request) {
	s.uiSetClonePower(w, r, true)
}
func (s *Server) uiStopCloneVM(w http.ResponseWriter, r *http.Request) {
	s.uiSetClonePower(w, r, false)
}

// --- console page ---

// consolePageView is the data for the console.html template: the power
// widget's poll URL, present only when reached via a clone VM's console
// link (?clone=&vm=), since build VMs have no power controls.
type consolePageView struct {
	PowerEndpoint string
}

// uiConsolePage renders console.html, adding the clone VM power-control
// widget when the request carries clone/vm query params.
func (s *Server) uiConsolePage(w http.ResponseWriter, r *http.Request) {
	var data consolePageView
	clone, vm := r.URL.Query().Get("clone"), r.URL.Query().Get("vm")
	if clone != "" && vm != "" {
		data.PowerEndpoint = "/ui/clones/" + clone + "/vms/" + vm + "/power"
	}
	s.renderFragment(w, "console.html.tmpl", data)
}

// --- grades ---

func (s *Server) uiListGrades(w http.ResponseWriter, r *http.Request) {
	views, err := s.fetchGrades(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing grades: "+err.Error())
		return
	}
	s.renderFragment(w, "grades-panel", views)
}

func (s *Server) uiCreateGrade(w http.ResponseWriter, r *http.Request) {
	manifest, err := formManifest(r)
	if err != nil {
		s.renderErrorMessage(w, http.StatusBadRequest, err.Error())
		return
	}
	var g v1alpha1.GradeRequest
	if err := yaml.Unmarshal([]byte(manifest), &g); err != nil {
		s.renderErrorMessage(w, http.StatusBadRequest, "parsing manifest: "+err.Error())
		return
	}
	if g.Namespace == "" {
		g.Namespace = s.namespace
	}
	if err := s.client.Create(r.Context(), &g); err != nil {
		status, msg := classifyCreateErr(err, "grade")
		if status != http.StatusConflict {
			msg += ": " + err.Error()
		}
		s.renderErrorMessage(w, status, msg)
		return
	}
	s.renderGradesPanel(w, r, &oobStatus{ID: "grade-status", Msg: "created " + g.Name})
}

func (s *Server) uiDeleteGrade(w http.ResponseWriter, r *http.Request) {
	g := v1alpha1.GradeRequest{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &g); err != nil && !apierrors.IsNotFound(err) {
		s.renderErrorMessage(w, http.StatusInternalServerError, "deleting grade: "+err.Error())
		return
	}
	s.renderGradesPanel(w, r, nil)
}

func (s *Server) renderGradesPanel(w http.ResponseWriter, r *http.Request, oob *oobStatus) {
	views, err := s.fetchGrades(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing grades: "+err.Error())
		return
	}
	s.renderPanel(w, "grades-panel", views, oob)
}
