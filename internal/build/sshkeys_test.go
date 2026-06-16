package build

import (
	"strings"
	"testing"
)

func TestInjectSSHKeyIntoCloudInit_EmptyArray(t *testing.T) {
	input := "#cloud-config\nssh_authorized_keys: []\n"
	key := []byte("ssh-ed25519 AAAA testkey")

	got := InjectSSHKeyIntoCloudInit(input, key)

	if strings.Contains(got, "ssh_authorized_keys: []") {
		t.Error("should have replaced empty array")
	}
	if !strings.Contains(got, "ssh-ed25519 AAAA testkey") {
		t.Error("should contain the injected key")
	}
	if !strings.Contains(got, "ssh_authorized_keys:") {
		t.Error("should contain ssh_authorized_keys block")
	}
}

func TestInjectSSHKeyIntoCloudInit_ExistingBlock(t *testing.T) {
	input := "#cloud-config\nssh_authorized_keys:\n  - ssh-rsa existing\n"
	key := []byte("ssh-ed25519 AAAA newkey")

	got := InjectSSHKeyIntoCloudInit(input, key)

	if !strings.Contains(got, "ssh-ed25519 AAAA newkey") {
		t.Error("should contain the new key")
	}
	if !strings.Contains(got, "ssh-rsa existing") {
		t.Error("should preserve existing key")
	}
}

func TestInjectSSHKeyIntoCloudInit_NoBlock(t *testing.T) {
	input := "#cloud-config\npackage_update: true\n"
	key := []byte("ssh-ed25519 AAAA testkey")

	got := InjectSSHKeyIntoCloudInit(input, key)

	if !strings.Contains(got, "ssh_authorized_keys:") {
		t.Error("should add ssh_authorized_keys block")
	}
	if !strings.Contains(got, "ssh-ed25519 AAAA testkey") {
		t.Error("should contain the injected key")
	}
	if !strings.Contains(got, "package_update: true") {
		t.Error("should preserve existing content")
	}
}

func TestInjectSSHKeyIntoCloudInit_TrimsWhitespace(t *testing.T) {
	input := "#cloud-config\nssh_authorized_keys: []\n"
	key := []byte("ssh-ed25519 AAAA testkey\n")

	got := InjectSSHKeyIntoCloudInit(input, key)

	// Should not have trailing newline in the key itself.
	if strings.Contains(got, "testkey\n\n") {
		t.Error("should not have double newline after key")
	}
}
