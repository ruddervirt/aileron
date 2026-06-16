package build

import (
	"context"
	"fmt"
	"strings"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	sshSecretPrivateKeyField = "id_ed25519"
	sshSecretPublicKeyField  = "id_ed25519.pub"
)

// SSHKeySecretName returns the name of the Secret holding SSH keys for a build.
func SSHKeySecretName(buildID string) string {
	return fmt.Sprintf("%s-ssh", buildID)
}

// EnsureSSHKeySecret creates a Secret with an auto-generated SSH keypair for the build,
// or returns the existing keypair if the Secret already exists.
func EnsureSSHKeySecret(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild) (*SSHKeyPair, error) {
	logger := log.FromContext(ctx)
	secretName := SSHKeySecretName(BuildID(build))
	buildNS := BuildNS(build)

	// Check if it already exists.
	existing := &corev1.Secret{}
	err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: buildNS}, existing)
	if err == nil {
		return &SSHKeyPair{
			PrivateKeyPEM:       existing.Data[sshSecretPrivateKeyField],
			PublicKeyAuthorized: existing.Data[sshSecretPublicKeyField],
		}, nil
	}
	if !errors.IsNotFound(err) {
		return nil, fmt.Errorf("checking SSH key secret: %w", err)
	}

	// Generate a new keypair.
	logger.Info("Generating SSH keypair for build", "secret", secretName)
	keyPair, err := GenerateSSHKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generating SSH keypair: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: buildNS,
			Labels: map[string]string{
				LabelBuildID:        BuildID(build),
				LabelBuild:          build.Name,
				LabelBuildNamespace: build.Namespace,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			sshSecretPrivateKeyField: keyPair.PrivateKeyPEM,
			sshSecretPublicKeyField:  keyPair.PublicKeyAuthorized,
		},
	}

	if err := c.Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			// Re-fetch the existing secret (created by a concurrent reconcile).
			if getErr := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: buildNS}, existing); getErr == nil {
				return &SSHKeyPair{
					PrivateKeyPEM:       existing.Data[sshSecretPrivateKeyField],
					PublicKeyAuthorized: existing.Data[sshSecretPublicKeyField],
				}, nil
			}
		}
		return nil, fmt.Errorf("creating SSH key secret: %w", err)
	}

	return keyPair, nil
}

// GetSSHPrivateKey retrieves the SSH private key for a build.
// If the VM spec has a custom key secret, it uses that instead.
func GetSSHPrivateKey(ctx context.Context, c client.Client, build *v1alpha1.VirtualMachineBuild, vmSpec *v1alpha1.BuildVM) ([]byte, error) {
	buildNS := BuildNS(build)
	if vmSpec.Communicator.SSHPrivateKeySecret != nil {
		ref := vmSpec.Communicator.SSHPrivateKeySecret
		secret := &corev1.Secret{}
		// Custom key secrets are in the CR namespace (user-provided).
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: build.Namespace}, secret); err != nil {
			return nil, fmt.Errorf("getting custom SSH key secret %s: %w", ref.Name, err)
		}
		key, ok := secret.Data[ref.Key]
		if !ok {
			return nil, fmt.Errorf("key %q not found in secret %s", ref.Key, ref.Name)
		}
		return key, nil
	}

	// Use the auto-generated key (stored in build namespace).
	secretName := SSHKeySecretName(BuildID(build))
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: buildNS}, secret); err != nil {
		return nil, fmt.Errorf("getting SSH key secret %s: %w", secretName, err)
	}
	return secret.Data[sshSecretPrivateKeyField], nil
}

// InjectSSHKeyIntoCloudInit merges the SSH public key into cloud-init userData.
// It adds the key to ssh_authorized_keys if present, or appends the block.
func InjectSSHKeyIntoCloudInit(userData string, publicKey []byte) string {
	pubKeyStr := strings.TrimSpace(string(publicKey))

	// If there's already an ssh_authorized_keys block with empty array, replace it.
	if strings.Contains(userData, "ssh_authorized_keys: []") {
		return strings.Replace(userData,
			"ssh_authorized_keys: []",
			fmt.Sprintf("ssh_authorized_keys:\n  - %s", pubKeyStr),
			1)
	}

	// If there's an existing ssh_authorized_keys block, append to it.
	if strings.Contains(userData, "ssh_authorized_keys:") {
		return strings.Replace(userData,
			"ssh_authorized_keys:",
			fmt.Sprintf("ssh_authorized_keys:\n  - %s", pubKeyStr),
			1)
	}

	// No existing block — append one.
	return userData + fmt.Sprintf("\nssh_authorized_keys:\n  - %s\n", pubKeyStr)
}
