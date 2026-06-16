package vncbridge

import (
	"testing"

	"k8s.io/client-go/rest"
)

func TestKubeVirtVNCWebSocketURL(t *testing.T) {
	const (
		ns  = "ruddervirt-system"
		vmi = "abc123-builder"
	)
	wantPath := "/apis/subresources.kubevirt.io/v1/namespaces/" + ns +
		"/virtualmachineinstances/" + vmi + "/vnc"

	tests := []struct {
		name    string
		host    string
		want    string
		wantErr bool
	}{
		{
			name: "https host",
			host: "https://1.2.3.4:6443",
			want: "wss://1.2.3.4:6443" + wantPath,
		},
		{
			name: "http host downgrades to ws",
			host: "http://10.0.0.1:8080",
			want: "ws://10.0.0.1:8080" + wantPath,
		},
		{
			name: "bare host:port defaults to wss",
			host: "kubernetes.default.svc:443",
			want: "wss://kubernetes.default.svc:443" + wantPath,
		},
		{
			name:    "empty host errors",
			host:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := kubeVirtVNCWebSocketURL(&rest.Config{Host: tt.host}, ns, vmi)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for host %q, got %q", tt.host, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"abc123", "a", "build-id-1", "x9", "abc-def-123"}
	for _, s := range valid {
		if !validName(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}

	invalid := []string{
		"",          // empty
		"..",        // path traversal
		"a/b",       // separator
		"UPPER",     // uppercase
		"-leading",  // leading dash
		"trailing-", // trailing dash
		"has space", // space
		"q?x",       // query smuggling
	}
	for _, s := range invalid {
		if validName(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}
