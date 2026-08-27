// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"errors"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

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
	s.renderFragment(w, http.StatusOK, "builds-panel", views)
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
	s.renderBuildsPanel(w, r, &oobStatus{ID: "build-status", Msg: "created " + b.Name})
}

func (s *Server) uiDeleteBuild(w http.ResponseWriter, r *http.Request) {
	b := v1alpha1.VirtualMachineBuild{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &b); err != nil && !apierrors.IsNotFound(err) {
		s.renderErrorMessage(w, http.StatusInternalServerError, "deleting build: "+err.Error())
		return
	}
	s.renderBuildsPanel(w, r, nil)
}

func (s *Server) renderBuildsPanel(w http.ResponseWriter, r *http.Request, oob *oobStatus) {
	views, err := s.fetchBuilds(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing builds: "+err.Error())
		return
	}
	s.renderPanel(w, "builds-panel", views, oob)
}

// --- clones ---

func (s *Server) uiListClones(w http.ResponseWriter, r *http.Request) {
	views, err := s.fetchClones(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing clones: "+err.Error())
		return
	}
	s.renderFragment(w, http.StatusOK, "clones-panel", views)
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
	s.renderClonesPanel(w, r, &oobStatus{ID: "clone-status-" + templateName, Msg: "created " + name})
}

func (s *Server) uiDeleteClone(w http.ResponseWriter, r *http.Request) {
	c := v1alpha1.VirtualMachineClone{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &c); err != nil && !apierrors.IsNotFound(err) {
		s.renderErrorMessage(w, http.StatusInternalServerError, "deleting clone: "+err.Error())
		return
	}
	s.renderClonesPanel(w, r, nil)
}

func (s *Server) renderClonesPanel(w http.ResponseWriter, r *http.Request, oob *oobStatus) {
	views, err := s.fetchClones(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing clones: "+err.Error())
		return
	}
	s.renderPanel(w, "clones-panel", views, oob)
}

// --- grades ---

func (s *Server) uiListGrades(w http.ResponseWriter, r *http.Request) {
	views, err := s.fetchGrades(r.Context())
	if err != nil {
		s.renderErrorMessage(w, http.StatusInternalServerError, "listing grades: "+err.Error())
		return
	}
	s.renderFragment(w, http.StatusOK, "grades-panel", views)
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
