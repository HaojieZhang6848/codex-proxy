package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
upstreams:
  - name: openai
    base_url: https://api.openai.com/v1
    api_key: upstream-secret
models:
  gpt-5-codex:
    key: client-secret
    upstream: openai
    upstream_model: gpt-5-codex-2025
`

func TestParseModelMapping(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(cfg.Upstreams); got != 1 {
		t.Fatalf("upstreams = %d, want 1", got)
	}
	if got := len(cfg.Models); got != 1 {
		t.Fatalf("models = %d, want 1", got)
	}
	model := cfg.Models[0]
	if model.Name != "gpt-5-codex" || model.Key != "client-secret" ||
		model.Upstream != "openai" || model.UpstreamModel != "gpt-5-codex-2025" {
		t.Fatalf("unexpected model %+v", model)
	}
	// Defaults.
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", cfg.Server.Listen, DefaultListen)
	}
	if cfg.Server.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("max body = %d, want %d", cfg.Server.MaxBodyBytes, DefaultMaxBodyBytes)
	}
	if cfg.Upstreams[0].AuthHeader != "Authorization" || cfg.Upstreams[0].AuthScheme != "Bearer" {
		t.Errorf("unexpected auth defaults %+v", cfg.Upstreams[0])
	}
	if cfg.Upstreams[0].ConnectTimeout.Duration() != DefaultConnectTimeout {
		t.Errorf("connect timeout = %s", cfg.Upstreams[0].ConnectTimeout)
	}
	if !cfg.ExposeModelsEndpoint() {
		t.Error("models endpoint should be exposed by default")
	}
}

func TestParseModelListForm(t *testing.T) {
	cfg, err := Parse([]byte(`
upstreams:
  - name: only
    base_url: https://example.test
    api_key: k
models:
  - name: a
    key: ka
    upstream_model: remote-a
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Models[0].Upstream != "only" {
		t.Errorf("upstream should default to the single upstream, got %q", cfg.Models[0].Upstream)
	}
	if cfg.Models[0].UpstreamModel != "remote-a" {
		t.Errorf("upstream_model = %q", cfg.Models[0].UpstreamModel)
	}
}

func TestParseExpandsEnvironment(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "from-env")
	cfg, err := Parse([]byte(strings.Replace(minimal, "upstream-secret", "${TEST_UPSTREAM_KEY}", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Upstreams[0].APIKey != "from-env" {
		t.Fatalf("api_key = %q, want from-env", cfg.Upstreams[0].APIKey)
	}

	unset, err := Parse([]byte(strings.Replace(minimal, "upstream-secret", "${TEST_UNSET_KEY}", 1)))
	if err != nil {
		t.Fatalf("Parse with unset variable: %v", err)
	}
	warnings := unset.Warnings()
	if len(warnings) == 0 || !strings.Contains(warnings[0], "TEST_UNSET_KEY") {
		t.Fatalf("expected a warning about the unset variable, got %v", warnings)
	}
}

func TestParseDurations(t *testing.T) {
	cfg, err := Parse([]byte(minimal + `
server:
  listen: 127.0.0.1:8080
  shutdown_timeout: 3s
  idle_timeout: 90
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.ShutdownTimeout.Duration() != 3*time.Second {
		t.Errorf("shutdown timeout = %s, want 3s", cfg.Server.ShutdownTimeout)
	}
	if cfg.Server.IdleTimeout.Duration() != 90*time.Second {
		t.Errorf("idle timeout = %s, want 90s", cfg.Server.IdleTimeout)
	}
}

func TestParseRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"no upstreams": `
models:
  a: {key: k}
`,
		"unknown upstream": `
upstreams:
  - {name: a, base_url: https://example.test, api_key: k}
models:
  a: {key: k, upstream: b}
`,
		"model without key": `
upstreams:
  - {name: a, base_url: https://example.test, api_key: k}
models:
  a: {upstream: a}
`,
		"duplicate model": `
upstreams:
  - {name: a, base_url: https://example.test, api_key: k}
models:
  a: {key: k}
  a: {key: k2}
`,
		"bad base url": `
upstreams:
  - {name: a, base_url: "ftp://example.test", api_key: k}
models:
  a: {key: k}
`,
		"bad proxy scheme": `
proxy: {url: "gopher://127.0.0.1:70"}
upstreams:
  - {name: a, base_url: https://example.test, api_key: k}
models:
  a: {key: k}
`,
		"ipv6 listen": `
server: {listen: "[::1]:8080"}
upstreams:
  - {name: a, base_url: https://example.test, api_key: k}
models:
  a: {key: k}
`,
		"unknown field": `
upstreams:
  - {name: a, base_url: https://example.test, api_key: k, typo: 1}
models:
  a: {key: k}
`,
		"bad duration": `
server: {shutdown_timeout: "soon"}
upstreams:
  - {name: a, base_url: https://example.test, api_key: k}
models:
  a: {key: k}
`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestResolve(t *testing.T) {
	cfg, err := Parse([]byte(`
upstreams:
  - name: one
    base_url: https://one.test
    api_key: k1
  - name: two
    base_url: https://two.test
    api_key: k2
models:
  alpha:
    key: key-alpha
    upstream: one
    upstream_model: alpha-remote
  beta:
    key: key-beta
    upstream: two
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	model, err := cfg.Resolve("alpha", "key-alpha")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if model.Upstream != "one" || model.UpstreamModel != "alpha-remote" {
		t.Fatalf("unexpected model %+v", model)
	}

	if _, err := cfg.Resolve("alpha", "key-beta"); err == nil {
		t.Error("expected a rejection for a key that belongs to another model")
	} else if resolveErr := err.(*ResolveError); resolveErr.Status != StatusUnauthorized {
		t.Errorf("status = %d, want %d", resolveErr.Status, StatusUnauthorized)
	}

	if _, err := cfg.Resolve("gamma", "key-alpha"); err == nil {
		t.Error("expected an error for an unknown model")
	} else if resolveErr := err.(*ResolveError); resolveErr.Status != StatusNotFound {
		t.Errorf("status = %d, want %d", resolveErr.Status, StatusNotFound)
	}

	// No model in the body: the key decides, and it is unambiguous here.
	model, err = cfg.Resolve("", "key-beta")
	if err != nil {
		t.Fatalf("Resolve by key: %v", err)
	}
	if model.Name != "beta" {
		t.Fatalf("model = %q, want beta", model.Name)
	}

	if _, err := cfg.Resolve("", "nope"); err == nil {
		t.Error("expected an error for an unknown key")
	}
	if !cfg.KnownKey("key-alpha") || cfg.KnownKey("nope") {
		t.Error("KnownKey returned the wrong answer")
	}
	if names := cfg.ModelNames(); len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("ModelNames = %v", names)
	}
}

func TestResolveAmbiguousKey(t *testing.T) {
	cfg, err := Parse([]byte(`
upstreams:
  - name: one
    base_url: https://one.test
    api_key: k1
  - name: two
    base_url: https://two.test
    api_key: k2
models:
  alpha: {key: shared, upstream: one}
  beta: {key: shared, upstream: two}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := cfg.Resolve("", "shared"); err == nil {
		t.Fatal("expected an ambiguity error")
	} else if resolveErr := err.(*ResolveError); resolveErr.Status != StatusBadRequest {
		t.Errorf("status = %d, want %d", resolveErr.Status, StatusBadRequest)
	}
	// With a model name the request is unambiguous again.
	if _, err := cfg.Resolve("beta", "shared"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}
