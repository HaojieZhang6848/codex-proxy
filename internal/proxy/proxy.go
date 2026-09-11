// Package proxy forwards Responses API requests to the configured upstreams.
//
// Only two things are changed on the way through: the client credential is
// replaced by the upstream credential, and the "model" field of the request
// body is replaced by the model name the mapping selects. Everything else,
// including paths, query strings, headers and streaming response bodies, is
// forwarded untouched.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/haojiezhang/codex-proxy/internal/config"
)

// Proxy is the http.Handler that implements the mapping.
type Proxy struct {
	cfg     *config.Config
	clients map[string]*http.Client
	log     *slog.Logger
}

// New builds a proxy for a validated configuration.
func New(cfg *config.Config, logger *slog.Logger) (*Proxy, error) {
	if logger == nil {
		logger = slog.Default()
	}
	selectProxy, err := proxyFunc(cfg.Proxy.URL)
	if err != nil {
		return nil, err
	}
	clients := make(map[string]*http.Client, len(cfg.Upstreams))
	for i := range cfg.Upstreams {
		up := &cfg.Upstreams[i]
		clients[up.Name] = newClient(up, selectProxy)
	}
	return &Proxy{cfg: cfg, clients: clients, log: logger}, nil
}

// requestInfo carries what is worth logging about a request.
type requestInfo struct {
	Model    string
	Upstream string
	Routed   string
}

// appName identifies this proxy in locally generated responses.
const appName = "codex-proxy"

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	info := p.forward(recorder, r)

	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", recorder.status,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	if info.Model != "" {
		attrs = append(attrs, "model", info.Model)
	}
	if info.Upstream != "" {
		attrs = append(attrs, "upstream", info.Upstream, "upstream_model", info.Routed)
	}
	if recorder.err != nil {
		attrs = append(attrs, "error", recorder.err.Error())
		p.log.Warn("request failed", attrs...)
		return
	}
	p.log.Info("request", attrs...)
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) *requestInfo {
	if p.cfg.ExposeModelsEndpoint() && isModelsRequest(r) {
		return p.serveModelList(w, r)
	}

	body, err := readBody(w, r, p.cfg.Server.MaxBodyBytes)
	if err != nil {
		return fail(w, nil, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
	}
	if err := checkContentEncoding(r); err != nil {
		return fail(w, nil, http.StatusUnsupportedMediaType, "unsupported_content_encoding", err.Error())
	}

	requestedModel, _ := modelNameOf(body)
	info := &requestInfo{Model: requestedModel}
	target, err := p.cfg.Resolve(requestedModel, clientKey(r))
	if err != nil {
		var resolveErr *config.ResolveError
		if errors.As(err, &resolveErr) {
			return fail(w, info, resolveErr.Status, resolveErr.Code, resolveErr.Message)
		}
		return fail(w, info, http.StatusInternalServerError, "internal_error", err.Error())
	}
	upstream, ok := p.cfg.Upstream(target.Upstream)
	if !ok {
		return fail(w, info, http.StatusInternalServerError, "internal_error",
			fmt.Sprintf("upstream %q is not configured", target.Upstream))
	}
	info.Upstream = upstream.Name
	info.Routed = target.UpstreamModel

	sendBody := body
	if requestedModel != "" && requestedModel != target.UpstreamModel {
		rewritten, changed, err := rewriteModel(body, target.UpstreamModel)
		if err != nil {
			return fail(w, info, http.StatusBadRequest, "invalid_request_body",
				"cannot read the model name from the request body: "+err.Error())
		}
		if changed {
			sendBody = rewritten
		}
	}

	targetURL, err := joinURL(upstream, r.URL)
	if err != nil {
		return fail(w, info, http.StatusInternalServerError, "internal_error", err.Error())
	}

	var reader io.Reader
	if sendBody != nil {
		reader = bytes.NewReader(sendBody)
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, reader)
	if err != nil {
		return fail(w, info, http.StatusInternalServerError, "internal_error", err.Error())
	}
	forwardHeaders(outReq.Header, r.Header)
	applyUpstreamHeaders(outReq.Header, upstream)
	if sendBody != nil {
		outReq.ContentLength = int64(len(sendBody))
	}
	setUpstreamAuth(outReq.Header, upstream)

	client := p.clients[upstream.Name]
	if client == nil {
		return fail(w, info, http.StatusInternalServerError, "internal_error",
			fmt.Sprintf("no client for upstream %q", upstream.Name))
	}

	resp, err := client.Do(outReq)
	if err != nil {
		return fail(w, info, statusFromError(err), "upstream_error",
			fmt.Sprintf("request to upstream %q failed: %v", upstream.Name, err))
	}
	defer resp.Body.Close()

	responseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if err := copyStream(w, resp.Body); err != nil {
		// The status has already been sent; the client sees a truncated body,
		// which is the only signal left.
		http.NewResponseController(w).Flush()
		p.log.Warn("streaming the upstream response failed",
			"upstream", upstream.Name, "error", err.Error())
	}
	return info
}

// clientKey extracts the credential the client presented.
func clientKey(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("Authorization")); value != "" {
		if len(value) > 7 && strings.EqualFold(value[:7], "bearer ") {
			return strings.TrimSpace(value[7:])
		}
		return value
	}
	for _, header := range []string{"api-key", "x-api-key"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}
	return ""
}

// applyUpstreamHeaders sets the headers configured for the upstream. They win
// over the headers of the incoming request.
func applyUpstreamHeaders(header http.Header, upstream *config.Upstream) {
	for name, value := range upstream.Headers {
		header.Set(name, value)
	}
}

// setUpstreamAuth replaces the client credential with the upstream credential.
func setUpstreamAuth(header http.Header, upstream *config.Upstream) {
	header.Del("Authorization")
	header.Del("Api-Key")
	header.Del("X-Api-Key")
	if upstream.APIKey == "" {
		return
	}
	value := upstream.APIKey
	if upstream.AuthScheme != "" {
		value = upstream.AuthScheme + " " + upstream.APIKey
	}
	header.Set(upstream.AuthHeader, value)
}

// forwardHeaders copies the client headers, minus the hop-by-hop ones and minus
// the credentials that were just replaced.
func forwardHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Authorization") ||
			strings.EqualFold(key, "Api-Key") || strings.EqualFold(key, "X-Api-Key") {
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		dst[http.CanonicalHeaderKey(key)] = copied
	}
}

// responseHeaders copies the upstream headers to the client, minus the
// hop-by-hop ones.
func responseHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		dst[http.CanonicalHeaderKey(key)] = copied
	}
}

func isHopByHopHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// checkContentEncoding refuses compressed bodies: the model name could not be
// rewritten inside them without decompressing, which would change the request.
func checkContentEncoding(r *http.Request) error {
	encoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return nil
	}
	if r.ContentLength == 0 {
		return nil
	}
	return fmt.Errorf("a request body with Content-Encoding: %s cannot be rewritten; send the body uncompressed", encoding)
}

// joinURL appends the incoming path and query to the upstream base url.
//
// Clients usually talk to "http://proxy/v1", so the incoming path starts with
// "/v1", while an upstream base url often ends with "/v1" as well. That one
// repeated segment is collapsed, so "https://api.openai.com" and
// "https://api.openai.com/v1" both end up at "https://api.openai.com/v1/responses".
func joinURL(upstream *config.Upstream, incoming *url.URL) (string, error) {
	base, err := url.Parse(upstream.BaseURL)
	if err != nil {
		return "", fmt.Errorf("upstream %q: invalid base_url: %w", upstream.Name, err)
	}
	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	incomingPath := incoming.EscapedPath()
	if incomingPath == "" {
		incomingPath = "/"
	}
	if !strings.HasPrefix(incomingPath, "/") {
		incomingPath = "/" + incomingPath
	}
	if head, rest := firstSegment(incomingPath); head != "" && head == lastSegment(basePath) {
		incomingPath = rest
	}

	rawPath := basePath + incomingPath
	if rawPath == "" {
		rawPath = "/"
	}

	target := &url.URL{Scheme: base.Scheme, Host: base.Host, User: base.User}
	if decoded, err := url.PathUnescape(rawPath); err == nil {
		target.Path = decoded
		if decoded != rawPath {
			target.RawPath = rawPath
		}
	} else {
		target.Path = rawPath
	}
	target.RawQuery = incoming.RawQuery
	return target.String(), nil
}

// firstSegment splits "/v1/responses" into "v1" and "/responses".
func firstSegment(path string) (head, rest string) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", ""
	}
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[:i], trimmed[i:]
	}
	return trimmed, ""
}

// lastSegment returns "v1" for "/openai/v1" and "" for "/".
func lastSegment(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// copyStream forwards the response body, flushing after every chunk so that
// server sent events reach the client immediately.
func copyStream(w http.ResponseWriter, body io.Reader) error {
	controller := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			_ = controller.Flush()
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// fail writes an OpenAI style error response. info carries whatever is already
// known about the request so that the log line stays useful.
func fail(w http.ResponseWriter, info *requestInfo, status int, code, message string) *requestInfo {
	writeAPIError(w, status, code, message)
	if info == nil {
		info = &requestInfo{}
	}
	return info
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Codex-Proxy-Error", code)
	w.WriteHeader(status)
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType(status),
			"code":    code,
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func errorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "invalid_request_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

func statusFromError(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusBadGateway
}

func isModelsRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/v1/models", "/models":
		return true
	}
	return false
}

// statusRecorder remembers the status code and keeps http.ResponseController
// working through Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
	err    error
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.wrote {
		return
	}
	s.wrote = true
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	if err != nil {
		s.err = err
	}
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
