package build

import (
	"context"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sScriptLoader loads script content from ConfigMaps or Secrets.
type K8sScriptLoader struct {
	Client    client.Client
	Namespace string
}

func (l *K8sScriptLoader) LoadScript(ctx context.Context, ref *v1alpha1.ConfigMapOrSecretReference, namespace string) (string, error) {
	ns := namespace
	if ns == "" {
		ns = l.Namespace
	}

	if ref.ConfigMapRef != nil {
		cm := &corev1.ConfigMap{}
		cmName := ref.ConfigMapRef.Name
		if err := l.Client.Get(ctx, types.NamespacedName{Name: cmName, Namespace: ns}, cm); err != nil {
			return "", fmt.Errorf("getting ConfigMap %s/%s: %w", ns, cmName, err)
		}
		key := ref.ConfigMapRef.Key
		data, ok := cm.Data[key]
		if !ok {
			return "", fmt.Errorf("key %q not found in ConfigMap %s/%s", key, ns, cmName)
		}
		return data, nil
	}

	if ref.SecretRef != nil {
		secret := &corev1.Secret{}
		secretName := ref.SecretRef.Name
		if err := l.Client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, secret); err != nil {
			return "", fmt.Errorf("getting Secret %s/%s: %w", ns, secretName, err)
		}
		key := ref.SecretRef.Key
		data, ok := secret.Data[key]
		if !ok {
			return "", fmt.Errorf("key %q not found in Secret %s/%s", key, ns, secretName)
		}
		return string(data), nil
	}

	return "", fmt.Errorf("neither configMapRef nor secretRef specified")
}
