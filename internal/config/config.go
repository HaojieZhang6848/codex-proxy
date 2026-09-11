// Package config loads, validates and indexes the codex-proxy configuration.
//
// The configuration describes three things:
//
//  1. upstreams        - remote Responses API endpoints (url, api key, which
//     model to ask for is decided by the mapping below).
//  2. models           - the models this proxy offers to its own clients
//     (model name + api key that clients must present).
//  3. the mapping      - every locally offered model points at one upstream
//     and names the model to request there.
package config

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvConfigPath points at the configuration file when -config is not given.
const EnvConfigPath = "CODEX_PROXY_CONFIG"

// DefaultListen is only reachable over IPv4 as well.
const DefaultListen = "0.0.0.0:8080"

// Defaults applied when a value is absent from the configuration file.
const (
	DefaultConnectTimeout        = 15 * time.Second
	DefaultResponseHeaderTimeout = 120 * time.Second
	DefaultReadHeaderTimeout     = 30 * time.Second
	DefaultIdleTimeout           = 120 * time.Second
	DefaultShutdownTimeout       = 15 * time.Second
	DefaultAuthHeader            = "Authorization"
	DefaultAuthScheme            = "Bearer"
	DefaultLogLevel              = "info"
	DefaultLogFormat             = "text"
	// DefaultMaxBodyBytes is generous because prompts may embed images.
	DefaultMaxBodyBytes int64 = 256 << 20
)

// Config is the whole configuration file.
type Config struct {
	Server    Server     `yaml:"server"`
	Proxy     Proxy      `yaml:"proxy"`
	Upstreams []Upstream `yaml:"upstreams"`
	Models    ModelList  `yaml:"models"`
	Log       Log        `yaml:"log"`

	path     string
	warnings []string
}

// Server configures the local listener.
type Server struct {
	Listen string `yaml:"listen"`
	// ReadHeaderTimeout / IdleTimeout / ShutdownTimeout are durations such as
	// "30s". There is deliberately no write timeout: responses are streamed.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	// ExposeModelsEndpoint answers GET /v1/models locally. Defaults to true.
	ExposeModelsEndpoint *bool `yaml:"expose_models_endpoint"`
	// MaxBodyBytes caps the request body size. Defaults to DefaultMaxBodyBytes.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// Proxy configures how outbound requests reach the upstream. An empty url means
// "use the system proxy" (HTTP_PROXY / HTTPS_PROXY / NO_PROXY).
type Proxy struct {
	URL string `yaml:"url"`
}

// Upstream is a remote endpoint that serves the Responses API.
type Upstream struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`

	// AuthHeader / AuthScheme describe how APIKey is sent. Defaults are
	// "Authorization" and "Bearer"; set auth_scheme to "" to send the raw key
	// (for example with auth_header: api-key).
	AuthHeader string `yaml:"auth_header"`
	AuthScheme string `yaml:"auth_scheme"`

	// Headers are sent with every request to this upstream and take precedence
	// over the headers of the incoming request. Use it for upstream specific
	// headers such as x-opencode-session or a fixed User-Agent. Credentials are
	// controlled by api_key, auth_header and auth_scheme, not here.
	Headers map[string]string `yaml:"headers"`

	ConnectTimeout        Duration `yaml:"connect_timeout"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
}

// Model is a model this proxy offers together with the client key that guards
// it and the upstream model it is mapped onto.
type Model struct {
	Name          string `yaml:"name"`
	Key           string `yaml:"key"`
	Upstream      string `yaml:"upstream"`
	UpstreamModel string `yaml:"upstream_model"`
}

// ModelList accepts either a YAML list or a YAML mapping keyed by model name.
type ModelList []Model

// UnmarshalYAML implements yaml.Unmarshaler for both supported shapes.
func (l *ModelList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		var items []Model
		if err := node.Decode(&items); err != nil {
			return err
		}
		*l = items
		return nil
	case yaml.MappingNode:
		var items []Model
		for i := 0; i+1 < len(node.Content); i += 2 {
			var m Model
			if err := node.Content[i+1].Decode(&m); err != nil {
				return fmt.Errorf("model %q: %w", node.Content[i].Value, err)
			}
			if m.Name == "" {
				m.Name = node.Content[i].Value
			}
			items = append(items, m)
		}
		*l = items
		return nil
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil
		}
	}
	return fmt.Errorf("models must be a list or a mapping, got %s", kindName(node))
}

// Log configures the logger.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
	File   string `yaml:"file"`
}

// Duration is a time.Duration that unmarshals from strings like "30s" and from
// plain numbers interpreted as seconds.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	// A bare number is read as seconds: "idle_timeout: 90".
	if node.Tag == "!!int" || node.Tag == "!!float" {
		var seconds float64
		if err := node.Decode(&seconds); err != nil {
			return fmt.Errorf("expected a duration such as \"30s\": %w", err)
		}
		*d = Duration(time.Duration(seconds * float64(time.Second)))
		return nil
	}
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("expected a duration such as \"30s\": %w", err)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(v)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String returns the canonical duration string.
func (d Duration) String() string { return time.Duration(d).String() }

// Path returns the file the configuration was loaded from, if any.
func (c *Config) Path() string { return c.path }

// Warnings returns non-fatal problems found while loading the configuration.
func (c *Config) Warnings() []string { return c.warnings }

// ExposeModelsEndpoint reports whether GET /v1/models is answered locally.
func (c *Config) ExposeModelsEndpoint() bool {
	return c.Server.ExposeModelsEndpoint == nil || *c.Server.ExposeModelsEndpoint
}

// Load reads, expands and validates a configuration file. ${VAR} and $VAR are
// replaced with environment variables before parsing, which keeps secrets out
// of the file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.path = path
	return cfg, nil
}

// Parse validates configuration bytes.
func Parse(raw []byte) (*Config, error) {
	var missing []string
	expanded := os.Expand(string(raw), func(key string) string {
		if value, ok := os.LookupEnv(key); ok {
			return value
		}
		missing = append(missing, key)
		return ""
	})

	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if len(missing) > 0 {
		cfg.warnings = append(cfg.warnings, fmt.Sprintf(
			"environment variables referenced but not set: %s", strings.Join(dedupe(missing), ", ")))
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if strings.TrimSpace(c.Server.Listen) == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Server.ReadHeaderTimeout == 0 {
		c.Server.ReadHeaderTimeout = Duration(DefaultReadHeaderTimeout)
	}
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = Duration(DefaultIdleTimeout)
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = Duration(DefaultShutdownTimeout)
	}
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if strings.TrimSpace(c.Log.Level) == "" {
		c.Log.Level = DefaultLogLevel
	}
	if strings.TrimSpace(c.Log.Format) == "" {
		c.Log.Format = DefaultLogFormat
	}
	for i := range c.Upstreams {
		up := &c.Upstreams[i]
		if strings.TrimSpace(up.AuthHeader) == "" && strings.TrimSpace(up.AuthScheme) == "" {
			up.AuthHeader = DefaultAuthHeader
			up.AuthScheme = DefaultAuthScheme
		}
		if up.AuthHeader == "" {
			up.AuthHeader = DefaultAuthHeader
		}
		if up.ConnectTimeout == 0 {
			up.ConnectTimeout = Duration(DefaultConnectTimeout)
		}
		if up.ResponseHeaderTimeout == 0 {
			up.ResponseHeaderTimeout = Duration(DefaultResponseHeaderTimeout)
		}
	}
	for i := range c.Models {
		m := &c.Models[i]
		m.Name = strings.TrimSpace(m.Name)
		m.Upstream = strings.TrimSpace(m.Upstream)
		m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
		if m.UpstreamModel == "" {
			m.UpstreamModel = m.Name
		}
	}
}

func (c *Config) validate() error {
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("config: log.level %q must be debug, info, warn or error", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "text", "json":
	default:
		return fmt.Errorf("config: log.format %q must be text or json", c.Log.Format)
	}
	if err := c.validateServer(); err != nil {
		return err
	}
	if err := c.validateProxy(); err != nil {
		return err
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("config: at least one entry in \"upstreams\" is required")
	}

	upstreamNames := make(map[string]bool, len(c.Upstreams))
	for i := range c.Upstreams {
		up := &c.Upstreams[i]
		up.Name = strings.TrimSpace(up.Name)
		if up.Name == "" {
			return fmt.Errorf("config: upstreams[%d].name is required", i)
		}
		if upstreamNames[up.Name] {
			return fmt.Errorf("config: duplicate upstream name %q", up.Name)
		}
		upstreamNames[up.Name] = true
		if err := validateBaseURL(up); err != nil {
			return fmt.Errorf("config: upstream %q: %w", up.Name, err)
		}
		for name, value := range up.Headers {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("config: upstream %q: headers contains an empty header name", up.Name)
			}
			up.Headers[name] = value
		}
		if strings.TrimSpace(up.APIKey) == "" {
			c.warnings = append(c.warnings, fmt.Sprintf(
				"upstream %q has no api_key; requests will be forwarded without credentials", up.Name))
		}
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("config: at least one entry in \"models\" is required")
	}
	modelNames := make(map[string]bool, len(c.Models))
	for i := range c.Models {
		m := &c.Models[i]
		if m.Name == "" {
			return fmt.Errorf("config: models[%d].name is required", i)
		}
		if modelNames[m.Name] {
			return fmt.Errorf("config: duplicate model name %q", m.Name)
		}
		modelNames[m.Name] = true
		if m.Key == "" {
			return fmt.Errorf("config: model %q: key is required", m.Name)
		}
		if m.Upstream == "" {
			if len(c.Upstreams) == 1 {
				m.Upstream = c.Upstreams[0].Name
			} else {
				return fmt.Errorf("config: model %q: upstream is required (several upstreams are configured)", m.Name)
			}
		}
		if !upstreamNames[m.Upstream] {
			return fmt.Errorf("config: model %q references unknown upstream %q", m.Name, m.Upstream)
		}
	}
	return nil
}

func (c *Config) validateServer() error {
	host, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return fmt.Errorf("config: server.listen %q is not a host:port address: %w", c.Server.Listen, err)
	}
	if port == "" {
		return fmt.Errorf("config: server.listen %q has no port", c.Server.Listen)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("config: server.listen %q: port %q is not a number", c.Server.Listen, port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return fmt.Errorf("config: server.listen %q: use an IPv4 address or a host name (IPv6 is not supported)", c.Server.Listen)
	}
	return nil
}

func (c *Config) validateProxy() error {
	raw := strings.TrimSpace(c.Proxy.URL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("config: proxy.url %q is not a valid URL: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("config: proxy.url %q: unsupported scheme %q (use http, https, socks5 or socks5h)", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("config: proxy.url %q has no host", raw)
	}
	return nil
}

func validateBaseURL(up *Upstream) error {
	raw := strings.TrimSpace(up.BaseURL)
	if raw == "" {
		return fmt.Errorf("base_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("base_url %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("base_url %q has no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("base_url %q must not contain a query string or fragment", raw)
	}
	return nil
}

// Upstream returns the named upstream.
func (c *Config) Upstream(name string) (*Upstream, bool) {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i], true
		}
	}
	return nil, false
}

func kindName(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.ScalarNode:
		return "a scalar"
	default:
		return "an unexpected node"
	}
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// DefaultPath returns the platform specific configuration location.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "codex-proxy", "config.yaml")
	case "darwin":
		return "/usr/local/etc/codex-proxy/config.yaml"
	default:
		return "/etc/codex-proxy/config.yaml"
	}
}

// DiscoverPath resolves the configuration file the way the service does:
// explicit flag, then environment, then the working directory, then the
// platform default. It returns the first path that exists.
func DiscoverPath(flagValue string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("config file %s: %w", v, err)
		}
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv(EnvConfigPath)); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("config file %s (from %s): %w", v, EnvConfigPath, err)
		}
		return v, nil
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "config.yaml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	candidate := DefaultPath()
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", fmt.Errorf("no configuration file found: pass -config, set %s, or create %s", EnvConfigPath, candidate)
}

// ResolveStatus codes used by ResolveError.
const (
	StatusBadRequest   = http.StatusBadRequest
	StatusUnauthorized = http.StatusUnauthorized
	StatusNotFound     = http.StatusNotFound
)

// ResolveError is returned when a request cannot be mapped onto a local model.
type ResolveError struct {
	Status  int
	Code    string
	Message string
}

func (e *ResolveError) Error() string { return e.Message }
