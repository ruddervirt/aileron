// Package ui implements aileron-ui: a basic, self-hosted web interface for
// submitting VirtualMachineBuild / VirtualMachineClone manifests, watching
// their status, and opening VM consoles in the browser.
//
// It is deliberately minimal and UNAUTHENTICATED — it must only be exposed on
// a trusted network (the chart ships it as a NodePort Service). VM consoles are
// reached by reverse-proxying the browser's WebSocket to the cluster-internal
// vncgateway (see vncproxy.go); no JWT or proprietary signing key is involved.
package ui

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
)

//go:embed static
var staticFS embed.FS

// maxManifestBytes caps the size of a submitted manifest body.
const maxManifestBytes = 1 << 20 // 1 MiB

// Server holds the dependencies for the aileron-ui HTTP server.
type Server struct {
	client     client.Client
	clientset  kubernetes.Interface // for streaming pod logs; may be nil
	namespace  string               // namespace builds/clones are listed/created in
	gatewayURL string               // base ws:// URL of the cluster-internal vncgateway
	log        *slog.Logger
}

// NewServer constructs a Server. namespace is where builds and clones are
// listed and (by default) created; gatewayURL is the base ws:// URL of the
// cluster-internal vncgateway (e.g. ws://aileron-vncgateway.ns.svc:7778).
// clientset is used to stream coordinator pod logs (provisioner output).
func NewServer(c client.Client, clientset kubernetes.Interface, namespace, gatewayURL string, log *slog.Logger) *Server {
	return &Server{
		client:     c,
		clientset:  clientset,
		namespace:  namespace,
		gatewayURL: strings.TrimRight(gatewayURL, "/"),
		log:        log,
	}
}

// Handler returns the root HTTP handler wiring the API, the VNC proxy, and the
// embedded single-page frontend.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /api/builds", s.listBuilds)
	mux.HandleFunc("POST /api/builds", s.createBuild)
	mux.HandleFunc("GET /api/builds/{name}", s.getBuild)
	mux.HandleFunc("GET /api/builds/{name}/logs", s.buildLogs)
	mux.HandleFunc("DELETE /api/builds/{name}", s.deleteBuild)

	mux.HandleFunc("GET /api/clones", s.listClones)
	mux.HandleFunc("POST /api/clones", s.createClone)
	mux.HandleFunc("GET /api/clones/{name}", s.getClone)
	mux.HandleFunc("DELETE /api/clones/{name}", s.deleteClone)

	mux.HandleFunc("GET /api/grades", s.listGrades)
	mux.HandleFunc("POST /api/grades", s.createGrade)
	mux.HandleFunc("GET /api/grades/{name}", s.getGrade)
	mux.HandleFunc("DELETE /api/grades/{name}", s.deleteGrade)

	mux.HandleFunc("GET /vnc/{namespace}/{vmi}", s.proxyVNC)

	// Everything else: the embedded SPA (index.html, console.html, assets).
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// staticFS is compiled in, so this cannot fail at runtime.
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(sub))

	return mux
}

// --- console projections -------------------------------------------------

// consoleTarget is a (namespace, vmi) pair the frontend turns into a /vnc URL.
type consoleTarget struct {
	VMName    string `json:"vmName"`
	Namespace string `json:"namespace"`
	VMI       string `json:"vmi"`
}

// provisionerView is one provisioner step's status within a VM's build.
type provisionerView struct {
	Index    int32  `json:"index"`
	Type     string `json:"type"`
	Name     string `json:"name,omitempty"`
	Status   string `json:"status"`
	Duration string `json:"duration,omitempty"`
	Message  string `json:"message,omitempty"`
}

// buildVMView summarizes one VM's build progress and its provisioner steps.
type buildVMView struct {
	Name         string            `json:"name"`
	Phase        string            `json:"phase"`
	Provisioners []provisionerView `json:"provisioners"`
}

type buildView struct {
	Name           string          `json:"name"`
	Namespace      string          `json:"namespace"`
	Phase          string          `json:"phase"`
	Message        string          `json:"message"`
	BuildID        string          `json:"buildID"`
	StartTime      *metav1.Time    `json:"startTime,omitempty"`
	CompletionTime *metav1.Time    `json:"completionTime,omitempty"`
	Consoles       []consoleTarget `json:"consoles"`
	VMs            []buildVMView   `json:"vms"`
}

type cloneView struct {
	Name           string          `json:"name"`
	Namespace      string          `json:"namespace"`
	TemplateName   string          `json:"templateName"`
	Phase          string          `json:"phase"`
	Message        string          `json:"message"`
	CloneID        string          `json:"cloneID"`
	StartTime      *metav1.Time    `json:"startTime,omitempty"`
	CompletionTime *metav1.Time    `json:"completionTime,omitempty"`
	Consoles       []consoleTarget `json:"consoles"`
}

func projectBuild(b *v1alpha1.VirtualMachineBuild) buildView {
	v := buildView{
		Name:           b.Name,
		Namespace:      b.Namespace,
		Phase:          string(b.Status.Phase),
		Message:        b.Status.Message,
		BuildID:        b.Status.BuildID,
		StartTime:      b.Status.StartTime,
		CompletionTime: b.Status.CompletionTime,
		Consoles:       []consoleTarget{},
		VMs:            []buildVMView{},
	}
	ns := b.Status.BuildNamespace
	for _, vm := range b.Status.VMStatuses {
		// vmName is the live KubeVirt VM/VMI name; fall back to the
		// deterministic {buildID}-{name} if status hasn't filled it yet.
		vmi := vm.VMName
		if vmi == "" && b.Status.BuildID != "" {
			vmi = b.Status.BuildID + "-" + vm.Name
		}
		if ns != "" && vmi != "" {
			v.Consoles = append(v.Consoles, consoleTarget{VMName: vm.Name, Namespace: ns, VMI: vmi})
		}

		vmView := buildVMView{Name: vm.Name, Phase: string(vm.Phase), Provisioners: []provisionerView{}}
		for _, p := range vm.ProvisionerResults {
			pv := provisionerView{
				Index:   p.Index,
				Type:    string(p.Type),
				Name:    p.Name,
				Status:  p.Status,
				Message: p.Message,
			}
			if p.Duration != nil {
				pv.Duration = p.Duration.Duration.String()
			}
			vmView.Provisioners = append(vmView.Provisioners, pv)
		}
		v.VMs = append(v.VMs, vmView)
	}
	return v
}

// gradeVMView is a per-VM grading result summary.
type gradeVMView struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	JobName string `json:"jobName,omitempty"`
	Message string `json:"message"`
}

type gradeView struct {
	Name            string        `json:"name"`
	Namespace       string        `json:"namespace"`
	Phase           string        `json:"phase"`
	Message         string        `json:"message"`
	TargetNamespace string        `json:"targetNamespace"`
	StartedAt       *metav1.Time  `json:"startedAt,omitempty"`
	CompletedAt     *metav1.Time  `json:"completedAt,omitempty"`
	VMs             []gradeVMView `json:"vms"`
}

func projectGrade(g *v1alpha1.GradeRequest) gradeView {
	v := gradeView{
		Name:            g.Name,
		Namespace:       g.Namespace,
		Phase:           string(g.Status.Phase),
		Message:         g.Status.Message,
		TargetNamespace: g.Spec.Namespace,
		StartedAt:       g.Status.StartedAt,
		CompletedAt:     g.Status.CompletedAt,
		VMs:             []gradeVMView{},
	}
	for _, vm := range g.Status.VMStatuses {
		v.VMs = append(v.VMs, gradeVMView{
			Name:    vm.Name,
			Phase:   string(vm.Phase),
			JobName: vm.JobName,
			Message: vm.Message,
		})
	}
	return v
}

func projectClone(c *v1alpha1.VirtualMachineClone) cloneView {
	v := cloneView{
		Name:           c.Name,
		Namespace:      c.Namespace,
		TemplateName:   c.Spec.TemplateName,
		Phase:          string(c.Status.Phase),
		Message:        c.Status.Message,
		CloneID:        c.Status.CloneID,
		StartTime:      c.Status.StartTime,
		CompletionTime: c.Status.CompletionTime,
		Consoles:       []consoleTarget{},
	}
	ns := c.Status.CloneNamespace
	for _, vm := range c.Status.VMStatuses {
		if ns == "" || vm.Name == "" {
			continue
		}
		// ClonedVMStatus.Name is the cloned VM's name in the clone namespace,
		// which is also the VMI name.
		v.Consoles = append(v.Consoles, consoleTarget{VMName: vm.Name, Namespace: ns, VMI: vm.Name})
	}
	return v
}

// --- build handlers ------------------------------------------------------

func (s *Server) listBuilds(w http.ResponseWriter, r *http.Request) {
	var list v1alpha1.VirtualMachineBuildList
	if err := s.client.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		s.httpError(w, http.StatusInternalServerError, "listing builds", err)
		return
	}
	views := make([]buildView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, projectBuild(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getBuild(w http.ResponseWriter, r *http.Request) {
	var b v1alpha1.VirtualMachineBuild
	key := client.ObjectKey{Namespace: s.namespace, Name: r.PathValue("name")}
	if err := s.client.Get(r.Context(), key, &b); err != nil {
		s.getError(w, err, "build")
		return
	}
	writeJSON(w, http.StatusOK, &b)
}

func (s *Server) createBuild(w http.ResponseWriter, r *http.Request) {
	var b v1alpha1.VirtualMachineBuild
	if !s.decodeManifest(w, r, &b) {
		return
	}
	if b.Namespace == "" {
		b.Namespace = s.namespace
	}
	if err := s.client.Create(r.Context(), &b); err != nil {
		s.createError(w, err, "build")
		return
	}
	writeJSON(w, http.StatusCreated, projectBuild(&b))
}

func (s *Server) deleteBuild(w http.ResponseWriter, r *http.Request) {
	b := v1alpha1.VirtualMachineBuild{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &b); err != nil {
		s.getError(w, err, "build")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// coordinatorJobName mirrors internal/build.CoordinatorJobName
// ({buildID}-coordinator-{vmName}); replicated here to avoid importing the
// heavy internal/build package into the UI binary. Keep in sync with
// internal/build/coordinator_job.go.
func coordinatorJobName(buildID, vmName string) string {
	return fmt.Sprintf("%s-coordinator-%s", buildID, vmName)
}

// buildLogs streams the coordinator pod's logs for one of a build's VMs. The
// coordinator drives provisioning, so its log IS the provisioner output. The
// pod lives in the build namespace and is owned by the coordinator Job.
func (s *Server) buildLogs(w http.ResponseWriter, r *http.Request) {
	vm := r.URL.Query().Get("vm")
	if vm == "" {
		s.httpError(w, http.StatusBadRequest, "vm query parameter is required", nil)
		return
	}
	if s.clientset == nil {
		s.httpError(w, http.StatusServiceUnavailable, "log streaming not configured", nil)
		return
	}

	var b v1alpha1.VirtualMachineBuild
	key := client.ObjectKey{Namespace: s.namespace, Name: r.PathValue("name")}
	if err := s.client.Get(r.Context(), key, &b); err != nil {
		s.getError(w, err, "build")
		return
	}
	buildID := b.Status.BuildID
	ns := b.Status.BuildNamespace
	if ns == "" {
		ns = s.namespace
	}
	if buildID == "" {
		s.httpError(w, http.StatusConflict, "build has not started provisioning yet", nil)
		return
	}

	jobName := coordinatorJobName(buildID, vm)
	var pods corev1.PodList
	if err := s.client.List(r.Context(), &pods,
		client.InNamespace(ns),
		client.MatchingLabels{"batch.kubernetes.io/job-name": jobName},
	); err != nil {
		s.httpError(w, http.StatusInternalServerError, "listing coordinator pods", err)
		return
	}
	if len(pods.Items) == 0 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "no coordinator pod yet for VM \""+vm+"\" (provisioning not started)\n")
		return
	}
	// Prefer a failed pod (most informative); else the most recently created.
	pod := &pods.Items[len(pods.Items)-1]
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodFailed {
			pod = &pods.Items[i]
			break
		}
	}

	tail := int64(5000)
	stream, err := s.clientset.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: "coordinator",
		TailLines: &tail,
	}).Stream(r.Context())
	if err != nil {
		s.httpError(w, http.StatusInternalServerError, "streaming coordinator logs", err)
		return
	}
	defer func() { _ = stream.Close() }()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := io.Copy(w, stream); err != nil {
		s.log.Warn("log copy interrupted", "error", err)
	}
}

// --- clone handlers ------------------------------------------------------

func (s *Server) listClones(w http.ResponseWriter, r *http.Request) {
	var list v1alpha1.VirtualMachineCloneList
	if err := s.client.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		s.httpError(w, http.StatusInternalServerError, "listing clones", err)
		return
	}
	views := make([]cloneView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, projectClone(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getClone(w http.ResponseWriter, r *http.Request) {
	var c v1alpha1.VirtualMachineClone
	key := client.ObjectKey{Namespace: s.namespace, Name: r.PathValue("name")}
	if err := s.client.Get(r.Context(), key, &c); err != nil {
		s.getError(w, err, "clone")
		return
	}
	writeJSON(w, http.StatusOK, &c)
}

func (s *Server) createClone(w http.ResponseWriter, r *http.Request) {
	var c v1alpha1.VirtualMachineClone
	if !s.decodeManifest(w, r, &c) {
		return
	}
	if c.Namespace == "" {
		c.Namespace = s.namespace
	}
	if err := s.client.Create(r.Context(), &c); err != nil {
		s.createError(w, err, "clone")
		return
	}
	writeJSON(w, http.StatusCreated, projectClone(&c))
}

func (s *Server) deleteClone(w http.ResponseWriter, r *http.Request) {
	c := v1alpha1.VirtualMachineClone{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &c); err != nil {
		s.getError(w, err, "clone")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- grade handlers ------------------------------------------------------

func (s *Server) listGrades(w http.ResponseWriter, r *http.Request) {
	var list v1alpha1.GradeRequestList
	if err := s.client.List(r.Context(), &list, client.InNamespace(s.namespace)); err != nil {
		s.httpError(w, http.StatusInternalServerError, "listing grades", err)
		return
	}
	views := make([]gradeView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, projectGrade(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getGrade(w http.ResponseWriter, r *http.Request) {
	var g v1alpha1.GradeRequest
	key := client.ObjectKey{Namespace: s.namespace, Name: r.PathValue("name")}
	if err := s.client.Get(r.Context(), key, &g); err != nil {
		s.getError(w, err, "grade")
		return
	}
	writeJSON(w, http.StatusOK, &g)
}

func (s *Server) createGrade(w http.ResponseWriter, r *http.Request) {
	var g v1alpha1.GradeRequest
	if !s.decodeManifest(w, r, &g) {
		return
	}
	if g.Namespace == "" {
		g.Namespace = s.namespace
	}
	if err := s.client.Create(r.Context(), &g); err != nil {
		s.createError(w, err, "grade")
		return
	}
	writeJSON(w, http.StatusCreated, projectGrade(&g))
}

func (s *Server) deleteGrade(w http.ResponseWriter, r *http.Request) {
	g := v1alpha1.GradeRequest{ObjectMeta: metav1.ObjectMeta{Namespace: s.namespace, Name: r.PathValue("name")}}
	if err := s.client.Delete(r.Context(), &g); err != nil {
		s.getError(w, err, "grade")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers -------------------------------------------------------------

// decodeManifest reads a YAML or JSON manifest body into obj. It writes an
// error response and returns false on failure.
func (s *Server) decodeManifest(w http.ResponseWriter, r *http.Request, obj client.Object) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxManifestBytes))
	if err != nil {
		s.httpError(w, http.StatusBadRequest, "reading request body", err)
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		s.httpError(w, http.StatusBadRequest, "empty manifest", nil)
		return false
	}
	// sigs.k8s.io/yaml handles both YAML and JSON and honors the json tags.
	if err := yaml.Unmarshal(body, obj); err != nil {
		s.httpError(w, http.StatusBadRequest, "parsing manifest", err)
		return false
	}
	return true
}

func (s *Server) getError(w http.ResponseWriter, err error, kind string) {
	if apierrors.IsNotFound(err) {
		s.httpError(w, http.StatusNotFound, kind+" not found", nil)
		return
	}
	s.httpError(w, http.StatusInternalServerError, "operating on "+kind, err)
}

func (s *Server) createError(w http.ResponseWriter, err error, kind string) {
	switch {
	case apierrors.IsAlreadyExists(err):
		s.httpError(w, http.StatusConflict, kind+" already exists", nil)
	case apierrors.IsBadRequest(err) || apierrors.IsInvalid(err):
		s.httpError(w, http.StatusUnprocessableEntity, "invalid "+kind+" manifest", err)
	default:
		s.httpError(w, http.StatusInternalServerError, "creating "+kind, err)
	}
}

func (s *Server) httpError(w http.ResponseWriter, code int, msg string, err error) {
	if err != nil {
		s.log.Error(msg, "error", err, "code", code)
		msg = msg + ": " + err.Error()
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		// Response already partially written; nothing actionable left to do.
		_ = err
	}
}

// EnvOrDefault returns the value of the environment variable key, or fallback
// when it is unset or empty.
func EnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
