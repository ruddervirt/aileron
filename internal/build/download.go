package build

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxDownloadBytes caps the size of any file fetched via DownloadURL. The
// callers use this for floppy contents and inline provisioner scripts — KB to
// MB in practice — so 100 MB is generous while still bounding memory.
const maxDownloadBytes = 100 * 1024 * 1024

// downloadTimeout is the full-request deadline including body read. A single
// build may legitimately download a few MB from a public URL; anything that
// takes longer than this is almost certainly a hung server or slow-loris.
const downloadTimeout = 5 * time.Minute

// DownloadURL fetches the body at rawURL with guards against SSRF and
// resource exhaustion. The caller (controller or coordinator) runs with an
// in-cluster service account that can reach cluster IPs, node kubelet ports,
// and cloud-metadata endpoints — so a user-supplied URL in a build spec
// must not be allowed to coax the pod into fetching from those.
//
// Guards applied:
//   - only http/https
//   - hostname must not resolve to any loopback, link-local, unspecified, or
//     private-range IP (IPv4 or IPv6), which blocks 169.254.169.254 and the
//     RFC1918 cluster ranges alike
//   - overall client timeout
//   - response body capped by io.LimitReader
func DownloadURL(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q not allowed; only http/https", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("url has no host")
	}
	if err := checkHostAllowed(ctx, host); err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout: downloadTimeout,
		// Re-check the destination on every redirect — a permitted host could
		// otherwise return 302 to a link-local address.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return checkHostAllowed(req.Context(), req.URL.Hostname())
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDownloadBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxDownloadBytes)
	}
	return body, nil
}

func checkHostAllowed(ctx context.Context, host string) error {
	resolver := net.DefaultResolver
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if isRestrictedIP(a.IP) {
			return fmt.Errorf("host %s resolves to restricted address %s", host, a.IP)
		}
	}
	return nil
}

func isRestrictedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsPrivate() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// IPv4 CGNAT range 100.64.0.0/10 is commonly used inside cloud VPCs and
	// is not covered by net.IP.IsPrivate().
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}
