// SPDX-License-Identifier: GPL-3.0-only

// Package policy tests the chart's namespace-scope ValidatingAdmissionPolicy
// against a real API server (envtest). It runs in its own envtest instance
// because the policy is cluster-wide and would interfere with the controller
// suite, which creates objects in other namespaces.
package policy

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

const (
	operatorNS = "ruddervirt-system"
	foreignNS  = "other-team"
)

var templateLine = regexp.MustCompile(`(?m)^\s*\{\{-.*\}\}\s*$\n?`)

// renderPolicy turns the Helm template into plain YAML without needing the
// helm binary: it drops whole-line `{{- ... }}` directives (the version guard
// and the labels include) and substitutes the two inline values.
func renderPolicy(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "chart", "aileron", "templates", "namespace-policy.yaml"))
	if err != nil {
		t.Fatalf("read policy template: %v", err)
	}
	s := templateLine.ReplaceAllString(string(raw), "")
	s = strings.ReplaceAll(s, `{{ include "aileron.fullname" . }}`, "aileron")
	s = strings.ReplaceAll(s, `{{ .Release.Namespace }}`, operatorNS)
	if strings.Contains(s, "{{") {
		t.Fatalf("unrendered template directive left in policy:\n%s", s)
	}

	var objs []*unstructured.Unstructured
	for doc := range strings.SplitSeq(s, "\n---\n") {
		m := map[string]any{}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("parse policy document: %v", err)
		}
		if len(m) == 0 {
			continue
		}
		objs = append(objs, &unstructured.Unstructured{Object: m})
	}
	if len(objs) != 2 {
		t.Fatalf("got %d policy documents, want 2 (policy and binding)", len(objs))
	}
	return objs
}

func obj(kind, ns string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetAPIVersion("ruddervirt.io/v1alpha1")
	u.SetKind(kind)
	u.SetNamespace(ns)
	u.SetGenerateName("policy-test-")
	return u
}

func gradeRequest(ns, target string) *unstructured.Unstructured {
	return obj("GradeRequest", ns, map[string]any{
		"namespace": target,
		"vms":       []any{map[string]any{"name": "vm1", "commands": []any{"true"}, "user": "u", "password": "p"}},
	})
}

func build(ns string, source map[string]any) *unstructured.Unstructured {
	return obj("VirtualMachineBuild", ns, map[string]any{
		"vms": []any{map[string]any{
			"name":      "vm1",
			"source":    source,
			"resources": map[string]any{"cpu": "1", "memory": "1Gi"},
		}},
	})
}

func TestNamespaceScopePolicy(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make test`")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx := context.Background()

	for _, ns := range []string{operatorNS, foreignNS} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
	}
	for _, p := range renderPolicy(t) {
		if err := c.Create(ctx, p); err != nil {
			t.Fatalf("create %s: %v", p.GetKind(), err)
		}
	}

	// The API server loads new policies asynchronously. Wait until a known-bad
	// object is refused before running the cases.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := c.Create(ctx, gradeRequest(foreignNS, operatorNS))
		if err != nil && strings.Contains(err.Error(), "must be created in namespace") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy never took effect, last result: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	cases := []struct {
		name    string
		obj     *unstructured.Unstructured
		wantErr string // "" means the object must be accepted
	}{
		{"grade request in foreign namespace", gradeRequest(foreignNS, operatorNS), "must be created in namespace " + operatorNS},
		{"grade request targeting foreign namespace", gradeRequest(operatorNS, foreignNS), "GradeRequest spec.namespace must be " + operatorNS},
		{"grade request in operator namespace", gradeRequest(operatorNS, operatorNS), ""},
		{"clone in foreign namespace", obj("VirtualMachineClone", foreignNS, map[string]any{"templateName": "t"}), "must be created in namespace"},
		{"clone with foreign namespace override", obj("VirtualMachineClone", operatorNS, map[string]any{"templateName": "t", "namespace": foreignNS}), "spec.namespace must be empty or " + operatorNS},
		{"clone in operator namespace", obj("VirtualMachineClone", operatorNS, map[string]any{"templateName": "t"}), ""},
		{"vmns in foreign namespace", obj("VirtualMachineNamespace", foreignNS, map[string]any{"namespace": operatorNS}), "must be created in namespace"},
		{"build in foreign namespace", build(foreignNS, map[string]any{"buildRef": map[string]any{"name": "p"}}), "must be created in namespace"},
		{"build with foreign buildRef", build(operatorNS, map[string]any{"buildRef": map[string]any{"name": "p", "namespace": foreignNS}}), "buildRef.namespace and sourcePvc.namespace must be " + operatorNS},
		{"build with foreign sourcePvc", build(operatorNS, map[string]any{"sourcePvc": map[string]any{"name": "p", "namespace": foreignNS}}), "buildRef.namespace and sourcePvc.namespace must be " + operatorNS},
		{"build in operator namespace", build(operatorNS, map[string]any{"buildRef": map[string]any{"name": "p", "namespace": operatorNS}}), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Create(ctx, tc.obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
