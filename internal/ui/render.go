// SPDX-License-Identifier: GPL-3.0-only

package ui

import (
	"embed"
	"html/template"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//go:embed templates
var templatesFS embed.FS

var uiTemplates = template.Must(template.New("ui").Funcs(template.FuncMap{
	"badgeClass":    badgeClass,
	"displayTime":   displayTime,
	"consoleHref":   consoleHref,
	"vmConsoleHref": vmConsoleHref,
	"deref":         derefInt32,
	"add":           func(a, b int) int { return a + b },
	"sub":           func(a, b int) int { return a - b },
}).ParseFS(templatesFS, "templates/*.html.tmpl"))

// badgeClass maps a CR phase/status string to the CSS class used to color
// its status badge.
func badgeClass(phase string) string {
	switch phase {
	case "Succeeded", "Ready":
		return "ok"
	case "Failed":
		return "fail"
	default:
		return "warn"
	}
}

// displayTime returns the first non-zero time among times, formatted for
// display, or "" if none is set.
func displayTime(times ...*metav1.Time) string {
	for _, t := range times {
		if t != nil && !t.IsZero() {
			return t.Local().Format("2006-01-02 15:04:05")
		}
	}
	return ""
}

// consoleHref builds the /console.html link for a console target. Namespace,
// VMI, and VMName are Kubernetes object names (DNS-1123 charset), so no
// escaping is needed.
func consoleHref(c consoleTarget) string {
	return "/console.html?ns=" + c.Namespace + "&vmi=" + c.VMI + "&name=" + c.VMName
}

// vmConsoleHref builds the /console.html link for a cloneVMView.
func vmConsoleHref(v cloneVMView) string {
	return consoleHref(consoleTarget{VMName: v.VMName, Namespace: v.Namespace, VMI: v.VMI})
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// oobStatus renders an out-of-band status line (e.g. "created foo") into the
// element with the given id, alongside a panel fragment in the same response.
type oobStatus struct {
	ID  string
	Msg string
}

// renderFragment writes a single named template as the entire HTML response.
func (s *Server) renderFragment(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := uiTemplates.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("rendering fragment", "template", name, "error", err)
	}
}

// renderPanel writes a panel fragment (status 200) and, if oob is non-nil, an
// out-of-band status update alongside it in the same response body.
func (s *Server) renderPanel(w http.ResponseWriter, tmplName string, data any, oob *oobStatus) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := uiTemplates.ExecuteTemplate(w, tmplName, data); err != nil {
		s.log.Error("rendering panel", "template", tmplName, "error", err)
		return
	}
	if oob != nil {
		if err := uiTemplates.ExecuteTemplate(w, "status-oob", oob); err != nil {
			s.log.Error("rendering status", "error", err)
		}
	}
}

// renderErrorMessage writes a small HTML fragment carrying an error message,
// with the given (non-2xx) status code.
func (s *Server) renderErrorMessage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := uiTemplates.ExecuteTemplate(w, "error-message", msg); err != nil {
		s.log.Error("rendering error", "error", err)
	}
}
