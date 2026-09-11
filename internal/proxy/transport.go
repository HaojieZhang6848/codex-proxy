package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/haojiezhang/codex-proxy/internal/config"
)

// contextDialer is the subset of net.Dialer used here, so tests can observe the
// network that is being dialled.
type contextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// ipv4DialFunc turns any TCP dial into an IPv4 dial. Go resolves only A records
// for "tcp4", so upstream hosts that publish both A and AAAA records are never
// contacted over IPv6. When an HTTP proxy is used this applies to the
// connection to the proxy (with socks5h the name is resolved by the proxy).
func ipv4DialFunc(dialer contextDialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
			network = "tcp4"
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

// proxyFunc returns the proxy selector for upstream connections: the url from
// the configuration file when set, otherwise the system proxy (HTTP_PROXY,
// HTTPS_PROXY and NO_PROXY).
func proxyFunc(raw string) (func(*http.Request) (*url.URL, error), error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return http.ProxyFromEnvironment, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy.url %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy.url %q: unsupported scheme %q", raw, u.Scheme)
	}
	return http.ProxyURL(u), nil
}

// newClient builds one client per upstream so that connection timeouts and the
// connection pool are per upstream.
func newClient(up *config.Upstream, selectProxy func(*http.Request) (*url.URL, error)) *http.Client {
	dialer := &net.Dialer{
		Timeout:   up.ConnectTimeout.Duration(),
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		Proxy:                 selectProxy,
		DialContext:           ipv4DialFunc(dialer),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: up.ResponseHeaderTimeout.Duration(),
		// Encodings are negotiated by the client, not by this proxy: request and
		// response bodies are forwarded byte for byte.
		DisableCompression: true,
	}
	// No client timeout: responses are streamed and may stay open for minutes.
	// Cancellation comes from the incoming request context.
	return &http.Client{Transport: transport}
}
