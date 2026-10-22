package gateway

import (
	"net"
	"net/http"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func requestClientHost(r *http.Request) string {
	if r == nil {
		return ""
	}
	if host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); host != "" {
		return stripHostPort(strings.Split(host, ",")[0])
	}
	if host := strings.TrimSpace(r.Host); host != "" {
		return stripHostPort(host)
	}
	return ""
}

// requestClientIP extracts the caller's real IP. Cloudflare tunnel puts the
// original client IP in CF-Connecting-IP; generic proxies use X-Forwarded-For
// (first hop) or X-Real-IP; direct connections fall back to RemoteAddr.
//
// Those headers are only read when the TCP peer is a loopback/private address
// (the tunnel, a local reverse proxy, a LAN load balancer). Any public peer can
// set them freely, so trusting them there would let a client pick its own
// "source IP" — the key auth throttling, provider self-registration quotas and
// audit rows are all accounted to it.
func requestClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if !isTrustedProxyPeer(r.RemoteAddr) {
		return stripHostPort(strings.TrimSpace(r.RemoteAddr))
	}
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		if host := stripHostPort(ip); net.ParseIP(host) != nil {
			return host
		}
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		first := strings.TrimSpace(strings.Split(xff, ",")[0])
		if host := stripHostPort(first); net.ParseIP(host) != nil {
			return host
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		if host := stripHostPort(ip); net.ParseIP(host) != nil {
			return host
		}
	}
	return stripHostPort(strings.TrimSpace(r.RemoteAddr))
}

// isTrustedProxyPeer reports whether forward headers may be honoured for a
// connection whose TCP peer is remoteAddr. Unparseable peers are untrusted.
func isTrustedProxyPeer(remoteAddr string) bool {
	ip := net.ParseIP(stripHostPort(strings.TrimSpace(remoteAddr)))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func stripHostPort(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(hostport)
	if err == nil {
		return host
	}
	return hostport
}

// classifyAccessSource labels a request as local / lan / public based on Host
// and the configured public base URL (tunnel or custom domain).
func classifyAccessSource(clientHost string, publicBaseURL string) string {
	host := strings.ToLower(strings.TrimSpace(clientHost))
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return monitor.AccessSourceLocal
	}
	publicHost := ""
	if publicBaseURL != "" {
		publicHost = strings.ToLower(stripHostPort(strings.TrimPrefix(strings.TrimPrefix(publicBaseURL, "https://"), "http://")))
		if idx := strings.Index(publicHost, "/"); idx >= 0 {
			publicHost = publicHost[:idx]
		}
	}
	if publicHost != "" && (host == publicHost || strings.HasSuffix(host, "."+publicHost)) {
		return monitor.AccessSourcePublic
	}
	if strings.HasSuffix(host, ".trycloudflare.com") || strings.HasSuffix(host, ".cfargotunnel.com") {
		return monitor.AccessSourcePublic
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() {
			return monitor.AccessSourceLocal
		}
		if ip.IsPrivate() {
			return monitor.AccessSourceLAN
		}
		return monitor.AccessSourcePublic
	}
	// Hostname that is not the public domain is treated as LAN (e.g. mac.local).
	return monitor.AccessSourceLAN
}
