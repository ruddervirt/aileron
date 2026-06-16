package vncbridge

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
)

// dialKubeVirtVNC opens a WebSocket straight to the KubeVirt VNC subresource
// for the named VMI, authenticated with the in-cluster ServiceAccount token.
func dialKubeVirtVNC(ctx context.Context, restConfig *rest.Config, namespace, vmiName string) (*websocket.Conn, error) {
	wsURL, err := kubeVirtVNCWebSocketURL(restConfig, namespace, vmiName)
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{
		Subprotocols: []string{"binary"},
	}

	tlsConfig := &tls.Config{}
	if restConfig.Insecure {
		tlsConfig.InsecureSkipVerify = true
	}
	if restConfig.CAFile != "" || len(restConfig.CAData) > 0 {
		if pool, err := rest.TLSConfigFor(restConfig); err == nil && pool != nil {
			tlsConfig = pool
		}
	}
	dialer.TLSClientConfig = tlsConfig

	header := http.Header{}
	token := restConfig.BearerToken
	if token == "" && restConfig.BearerTokenFile != "" {
		data, err := os.ReadFile(restConfig.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading bearer token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}

	slog.Info("dialing KubeVirt VNC", "namespace", namespace, "vmi", vmiName, "upstream", wsURL)

	ws, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		detail := ""
		if resp != nil {
			detail = fmt.Sprintf(" (HTTP %d)", resp.StatusCode)
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("WebSocket dial %s%s: %w", wsURL, detail, err)
	}
	return ws, nil
}

// kubeVirtVNCWebSocketURL builds the WebSocket URL for a KubeVirt VNC
// subresource from the API server host in restConfig.
func kubeVirtVNCWebSocketURL(restConfig *rest.Config, namespace, vmiName string) (string, error) {
	host := restConfig.Host
	if host == "" {
		return "", fmt.Errorf("restConfig.Host is empty")
	}

	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("parsing restConfig.Host %q: %w", host, err)
	}
	// A bare "host:port" (no scheme) parses with an empty Host; re-parse it with
	// an https:// prefix so we get the authority in u.Host.
	if u.Host == "" {
		u, err = url.Parse("https://" + host)
		if err != nil {
			return "", fmt.Errorf("parsing restConfig.Host %q: %w", host, err)
		}
	}

	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}

	path := fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s/vnc",
		namespace, vmiName)

	return (&url.URL{Scheme: scheme, Host: u.Host, Path: path}).String(), nil
}
