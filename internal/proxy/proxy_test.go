package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/haojiezhang/codex-proxy/internal/config"
)

// newIPv4Server starts a test server on 127.0.0.1. httptest.NewServer would
// try to bind [::1] first, which fails where IPv6 is unavailable.
func newIPv4Server(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func newProxy(t *testing.T, upstreamURL, proxyURL string) *Proxy {
	t.Helper()
	raw := fmt.Sprintf(`
server:
  listen: 127.0.0.1:0
proxy:
  url: %q
upstreams:
  - name: up
    base_url: %s
    api_key: upstream-secret
models:
  local-model:
    key: client-secret
    upstream: up
    upstream_model: remote-model
log:
  level: error
`, proxyURL, upstreamURL)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestForwardRewritesModelAndKeyOnly(t *testing.T) {
	type captured struct {
		path          string
		query         string
		body          string
		authorization string
		apiKey        string
		userAgent     string
	}
	got := make(chan captured, 1)

	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			body:          string(body),
			authorization: r.Header.Get("Authorization"),
			apiKey:        r.Header.Get("Api-Key"),
			userAgent:     r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()

	p := newProxy(t, upstream.URL+"/v1", "")
	front := newIPv4Server(t, p)
	defer front.Close()

	body := `{"model":"local-model","input":"hello","stream":true}`
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses?trace=1", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Api-Key", "client-secret")
	req.Header.Set("User-Agent", "codex-cli/1.2.3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, responseBody)
	}
	if string(responseBody) != "data: {\"type\":\"response.completed\"}\n\n" {
		t.Fatalf("unexpected response body %q", responseBody)
	}

	seen := <-got
	if seen.path != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses (base url path must be preserved)", seen.path)
	}
	if seen.query != "trace=1" {
		t.Errorf("query = %q, want trace=1", seen.query)
	}
	if seen.body != `{"model":"remote-model","input":"hello","stream":true}` {
		t.Errorf("upstream body = %s", seen.body)
	}
	if seen.authorization != "Bearer upstream-secret" {
		t.Errorf("authorization = %q, want the upstream key", seen.authorization)
	}
	if seen.apiKey != "" {
		t.Errorf("the client api-key header must not be forwarded, got %q", seen.apiKey)
	}
	if seen.userAgent != "codex-cli/1.2.3" {
		t.Errorf("unrelated headers must be forwarded, got User-Agent %q", seen.userAgent)
	}
}

func TestForwardRejectsUnknownModelAndKey(t *testing.T) {
	var calls int
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newProxy(t, upstream.URL, "")
	front := newIPv4Server(t, p)
	defer front.Close()

	cases := []struct {
		name   string
		body   string
		key    string
		status int
		code   string
	}{
		{"unknown model", `{"model":"nope"}`, "client-secret", http.StatusNotFound, "model_not_found"},
		{"wrong key", `{"model":"local-model"}`, "wrong", http.StatusUnauthorized, "invalid_api_key"},
		{"no key at all", `{"model":"local-model"}`, "", http.StatusUnauthorized, "invalid_api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(tc.body))
			if tc.key != "" {
				req.Header.Set("Authorization", "Bearer "+tc.key)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			var payload struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if payload.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", payload.Error.Code, tc.code)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("the upstream must not be called for rejected requests, got %d calls", calls)
	}
}

func TestForwardUsesConfiguredProxy(t *testing.T) {
	var proxied []string
	proxy := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = append(proxied, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"via":"proxy"}`)
	}))
	defer proxy.Close()

	// The upstream host does not resolve: the request can only succeed when it
	// is sent through the configured proxy.
	p := newProxy(t, "http://upstream.invalid", proxy.URL)
	front := newIPv4Server(t, p)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"local-model"}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"via":"proxy"}` {
		t.Fatalf("body = %s", body)
	}
	if len(proxied) != 1 || !strings.HasPrefix(proxied[0], "http://upstream.invalid/v1/responses") {
		t.Fatalf("proxy saw %v", proxied)
	}
}

func TestForwardRejectsCompressedBody(t *testing.T) {
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the upstream must not be called")
	}))
	defer upstream.Close()

	p := newProxy(t, upstream.URL, "")
	front := newIPv4Server(t, p)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses",
		bytes.NewReader([]byte{0x1f, 0x8b, 0x08}))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}

func TestModelListEndpoint(t *testing.T) {
	p := newProxy(t, "http://127.0.0.1:1", "")
	front := newIPv4Server(t, p)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Data) != 1 || payload.Data[0].ID != "local-model" {
		t.Fatalf("unexpected model list %+v", payload)
	}

	bad, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
	bad.Header.Set("Authorization", "Bearer nope")
	badResp, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", badResp.StatusCode)
	}
}

func TestUpstreamHeadersOverrideClientHeaders(t *testing.T) {
	received := make(chan http.Header, 1)
	upstream := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))

	cfg, err := config.Parse([]byte(fmt.Sprintf(`
upstreams:
  - name: up
    base_url: %s
    api_key: upstream-secret
    headers:
      x-opencode-session: codex-local-opencode-go
      User-Agent: codex-opencode-go/1.0
models:
  local-model: {key: client-secret, upstream_model: remote-model}
`, upstream.URL)))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := newIPv4Server(t, p)

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/responses", strings.NewReader(`{"model":"local-model"}`))
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("User-Agent", "codex-cli/1.2.3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	header := <-received
	for name, want := range map[string]string{
		"X-Opencode-Session": "codex-local-opencode-go",
		"User-Agent":         "codex-opencode-go/1.0",
		"Authorization":      "Bearer upstream-secret",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
