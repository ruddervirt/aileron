package build

import (
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateSSHKeyPair(t *testing.T) {
	kp, err := GenerateSSHKeyPair()
	if err != nil {
		t.Fatalf("GenerateSSHKeyPair() error: %v", err)
	}

	if len(kp.PrivateKeyPEM) == 0 {
		t.Error("PrivateKeyPEM is empty")
	}
	if len(kp.PublicKeyAuthorized) == 0 {
		t.Error("PublicKeyAuthorized is empty")
	}

	// Verify the private key is parseable.
	signer, err := ssh.ParsePrivateKey(kp.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("cannot parse generated private key: %v", err)
	}

	// Verify the public key matches.
	pubKeyStr := strings.TrimSpace(string(kp.PublicKeyAuthorized))
	if !strings.HasPrefix(pubKeyStr, "ssh-ed25519 ") {
		t.Errorf("public key doesn't start with 'ssh-ed25519': %s", pubKeyStr)
	}

	// Verify the signer's public key type.
	if signer.PublicKey().Type() != "ssh-ed25519" {
		t.Errorf("unexpected key type: %s", signer.PublicKey().Type())
	}
}

func TestBuildNameForBuildVM(t *testing.T) {
	got := BuildNameForBuildVM("vm-abc123", "server")
	if got != "vm-abc123-server" {
		t.Errorf("got %s, want vm-abc123-server", got)
	}
}

func TestBuildNameForBuildVMDataVolume(t *testing.T) {
	got := BuildNameForBuildVMDataVolume("vm-abc123", "server")
	if got != "vm-abc123-src-server" {
		t.Errorf("got %s, want vm-abc123-src-server", got)
	}
}

func TestBuildNameForOutputDV(t *testing.T) {
	got := BuildNameForOutputDV("vm-abc123", "server")
	if got != "vm-abc123-out-server" {
		t.Errorf("got %s, want vm-abc123-out-server", got)
	}
}
