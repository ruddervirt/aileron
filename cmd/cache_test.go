// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ruddervirt/aileron/internal/network"
)

func TestManagerCacheOptionsScopesToOperatorNamespace(t *testing.T) {
	opts := managerCacheOptions("ruddervirt-system")

	if len(opts.DefaultNamespaces) != 1 {
		t.Fatalf("DefaultNamespaces = %v, want only ruddervirt-system", opts.DefaultNamespaces)
	}
	if _, ok := opts.DefaultNamespaces["ruddervirt-system"]; !ok {
		t.Fatalf("DefaultNamespaces = %v, want ruddervirt-system", opts.DefaultNamespaces)
	}

	if len(opts.ByObject) != 1 {
		t.Fatalf("ByObject has %d entries, want 1 (Pod)", len(opts.ByObject))
	}
	for obj, by := range opts.ByObject {
		if _, ok := obj.(*corev1.Pod); !ok {
			t.Fatalf("ByObject key %T, want *corev1.Pod", obj)
		}
		if len(by.Namespaces) != 2 {
			t.Fatalf("Pod namespaces = %v, want operator namespace and %s", by.Namespaces, network.OVNOVSNamespace)
		}
		if cfg, ok := by.Namespaces["ruddervirt-system"]; !ok || cfg.LabelSelector != nil {
			t.Fatalf("Pod cache in operator namespace = %+v, want present with no selector", cfg)
		}
		cfg, ok := by.Namespaces[network.OVNOVSNamespace]
		if !ok || cfg.LabelSelector == nil {
			t.Fatalf("Pod cache in %s = %+v, want app=ovs selector", network.OVNOVSNamespace, cfg)
		}
		if !cfg.LabelSelector.Matches(labels.Set{"app": "ovs"}) {
			t.Fatal("kube-system selector does not match app=ovs")
		}
		if cfg.LabelSelector.Matches(labels.Set{"app": "virt-launcher"}) {
			t.Fatal("kube-system selector matches non-OVS pods")
		}
	}
}
