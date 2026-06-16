package build

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"golang.org/x/crypto/ssh"
)

// SSHKeyPair holds a generated SSH keypair for build VM access.
type SSHKeyPair struct {
	PrivateKeyPEM       []byte
	PublicKeyAuthorized []byte
}

// GenerateSSHKeyPair creates a new Ed25519 SSH keypair.
func GenerateSSHKeyPair() (*SSHKeyPair, error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ed25519 key: %w", err)
	}

	privPEM, err := ssh.MarshalPrivateKey(privKey, "")
	if err != nil {
		return nil, fmt.Errorf("marshaling private key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, fmt.Errorf("creating SSH public key: %w", err)
	}

	return &SSHKeyPair{
		PrivateKeyPEM:       pem.EncodeToMemory(privPEM),
		PublicKeyAuthorized: ssh.MarshalAuthorizedKey(sshPub),
	}, nil
}

// BuildNameForBuildVM returns the ephemeral VM name for a given build + VM.
// buildID is the unique CUID2-based identifier (e.g. "vm-abc123").
func BuildNameForBuildVM(buildID, vmName string) string {
	return fmt.Sprintf("%s-%s", buildID, vmName)
}

// BuildNameForBuildVMDataVolume returns the source import DataVolume name.
func BuildNameForBuildVMDataVolume(buildID, vmName string) string {
	return fmt.Sprintf("%s-src-%s", buildID, vmName)
}

// BuildNameForOutputDV returns the output DataVolume name for a built VM.
func BuildNameForOutputDV(buildID, vmName string) string {
	return fmt.Sprintf("%s-out-%s", buildID, vmName)
}

// SourceCacheKey returns a deterministic cache key for a build source.
// URL sources use SHA-256 of the URL. PVC sources use namespace/name.
// Container disks use the image reference. Blank/buildRef are not cacheable.
func SourceCacheKey(src *resolvedSource) string {
	switch {
	case src.url != "":
		h := sha256.Sum256([]byte(src.url))
		return hex.EncodeToString(h[:])
	case src.containerDisk != "":
		h := sha256.Sum256([]byte(src.containerDisk))
		return hex.EncodeToString(h[:])
	default:
		return "" // not cacheable (blank, PVC, buildRef)
	}
}

// SourceCacheDVName returns the DataVolume name for a cached source image.
func SourceCacheDVName(cacheKey string) string {
	return fmt.Sprintf("aileron-src-%s", cacheKey[:16])
}

// ISOCacheKey returns the cache key for an ISO source.
// Uses the checksum if provided, otherwise SHA-256 of the URL.
func ISOCacheKey(iso *v1alpha1.ISOSource) string {
	if iso.Checksum != "" {
		return iso.Checksum
	}
	h := sha256.Sum256([]byte(iso.URL))
	return hex.EncodeToString(h[:])
}

// ISOCacheDVName returns the DataVolume name for a cached ISO image.
func ISOCacheDVName(cacheKey string) string {
	return fmt.Sprintf("aileron-iso-%s", cacheKey[:16])
}

// ISOCloneDVName returns the DataVolume name for a per-build clone of a
// cached ISO. The clone is what the VMI's launcher pod actually mounts —
// keeping the cached ISO un-attached so it can be cloned again for other
// concurrent builds. Naming is per (build, VM, isoIndex) so multiple
// concurrent builds and multiple ISOs per VM all coexist.
func ISOCloneDVName(buildID, vmName string, isoIdx int) string {
	return fmt.Sprintf("%s-%s-iso%d", buildID, vmName, isoIdx)
}
