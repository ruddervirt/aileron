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
	"badgeClass":       badgeClass,
	"displayTime":      displayTime,
	"consoleHref":      consoleHref,
	"cloneConsoleHref": cloneConsoleHref,
	"clonePowerWidget": clonePowerWidget,
	"pagerCtx":         pagerCtx,
	"deref":            derefInt32,
	"add":              func(a, b int) int { return a + b },
	"sub":              func(a, b int) int { return a - b },
}).ParseFS(templatesFS, "templates/*.html.tmpl"))

// badgeClass maps a CR phase/status string to the CSS class used to color
// its status badge.
func badgeClass(phase string) string {
	switch phase {
	case "Succeeded", "Ready", string(vmPowerRunning):
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

// cloneConsoleHref builds the /console.html link for a clone VM, additionally
// tagging it with the owning clone + VM name so the console page knows to
// show power controls for it (unlike a build VM's plain console link).
func cloneConsoleHref(cloneName string, v cloneVMView) string {
	return consoleHref(consoleTarget{VMName: v.VMName, Namespace: v.Namespace, VMI: v.VMI}) + "&clone=" + cloneName + "&vm=" + v.VMName
}

// clonePowerWidget builds the "power-widget" template's data for one clone
// VM row on the clone card. TargetID is unique per clone+VM (unlike
// console.html's fixed "power" id) since a card can show several VMs across
// several clones at once, each needing its own swap target.
func clonePowerWidget(cloneName string, v cloneVMView) powerWidgetView {
	return powerWidgetView{
		CloneName: cloneName,
		VMName:    v.VMName,
		Phase:     v.Phase,
		TargetID:  "vm-power-" + cloneName + "-" + v.VMName,
	}
}

// pagerCtx builds the "pager-oob" template's data.
func pagerCtx(container, endpoint string, page, totalPages int) pagerView {
	return pagerView{Container: container, Endpoint: endpoint, Page: page, TotalPages: totalPages}
}

type pagerView struct {
	Container  string
	Endpoint   string
	Page       int
	TotalPages int
}

// powerWidgetView is the "power-widget" template's data: one clone VM's live
// power phase, the clone/VM names its start/stop buttons act on, and the id
// of the element those buttons re-render into.
type powerWidgetView struct {
	CloneName string
	VMName    string
	Phase     string
	TargetID  string
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
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
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
