package proxy

import (
	"context"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/haojiezhang/codex-proxy/internal/config"
)

type recordingDialer struct {
	network string
	address string
}

func (d *recordingDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	d.network = network
	d.address = address
	return nil, net.ErrClosed
}

func TestIPv4DialFuncForcesTCP4(t *testing.T) {
	for _, network := range []string{"tcp", "tcp6", "tcp4"} {
		recorder := &recordingDialer{}
		dial := ipv4DialFunc(recorder)
		if _, err := dial(context.Background(), network, "example.test:443"); err == nil {
			t.Fatal("expected the dial to fail")
		}
		if recorder.network != "tcp4" {
			t.Errorf("network %q was dialled as %q, want tcp4", network, recorder.network)
		}
		if recorder.address != "example.test:443" {
			t.Errorf("address = %q", recorder.address)
		}
	}
}

func TestProxyFuncDefaultsToSystemProxy(t *testing.T) {
	// An empty proxy url means "use the system proxy", never "no proxy at all".
	if _, err := proxyFunc(""); err != nil {
		t.Fatalf("proxyFunc(\"\"): %v", err)
	}
	if _, err := proxyFunc("http://127.0.0.1:3128"); err != nil {
		t.Fatalf("proxyFunc(http): %v", err)
	}
	if _, err := proxyFunc("socks5://127.0.0.1:1080"); err != nil {
		t.Fatalf("proxyFunc(socks5): %v", err)
	}
	if _, err := proxyFunc("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("expected an error for an unsupported scheme")
	}
}

func TestJoinURL(t *testing.T) {
	cases := []struct {
		base     string
		incoming string
		want     string
	}{
		{"https://api.openai.com", "/v1/responses?a=1", "https://api.openai.com/v1/responses?a=1"},
		{"https://api.openai.com/v1", "/v1/responses", "https://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1/", "/v1/responses", "https://api.openai.com/v1/responses"},
		{"https://host.test/openai/v1", "/v1/responses", "https://host.test/openai/v1/responses"},
		{"https://host.test/openai", "/v1/responses", "https://host.test/openai/v1/responses"},
		{"https://host.test/v1", "/v2/responses", "https://host.test/v1/v2/responses"},
		{"https://host.test/v1", "/v1", "https://host.test/v1"},
		{"https://host.test/", "/v1/models", "https://host.test/v1/models"},
		{"https://host.test/v1", "/v1/responses/abc%2Fdef", "https://host.test/v1/responses/abc%2Fdef"},
	}
	for _, tc := range cases {
		upstream := &config.Upstream{Name: "up", BaseURL: tc.base}
		incoming, err := url.Parse(tc.incoming)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.incoming, err)
		}
		got, err := joinURL(upstream, incoming)
		if err != nil {
			t.Fatalf("joinURL(%q, %q): %v", tc.base, tc.incoming, err)
		}
		if got != tc.want {
			t.Errorf("joinURL(%q, %q) = %q, want %q", tc.base, tc.incoming, got, tc.want)
		}
	}
}

func TestIPv4DialFuncRejectsIPv6Literal(t *testing.T) {
	dial := ipv4DialFunc(&net.Dialer{Timeout: time.Second})
	if _, err := dial(context.Background(), "tcp", "[::1]:8080"); err == nil {
		t.Fatal("an IPv6 literal must not be dialled")
	}
}
