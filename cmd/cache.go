// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ruddervirt/aileron/internal/network"
)

// managerCacheOptions limits the manager's informers to the operator
// namespace, so Aileron never lists or watches namespaced objects (VMs, VMIs,
// pods, ...) that belong to other namespaces. Cluster-scoped kinds are not
// affected. The one exception is the kube-ovn OVS pods in kube-system, which
// the ipset code must list.
func managerCacheOptions(operatorNS string) cache.Options {
	return cache.Options{
		DefaultNamespaces: map[string]cache.Config{
			operatorNS: {},
		},
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {
				Namespaces: map[string]cache.Config{
					operatorNS: {},
					network.OVNOVSNamespace: {
						LabelSelector: labels.SelectorFromSet(network.OVNOVSPodLabels),
					},
				},
			},
		},
	}
}
