// Package server wires the configuration, the proxy and the HTTP listener.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"

	"github.com/haojiezhang/codex-proxy/internal/config"
	"github.com/haojiezhang/codex-proxy/internal/proxy"
)

// Server is the local HTTP server.
type Server struct {
	cfg  *config.Config
	log  *slog.Logger
	http *http.Server
}

// New builds the server, including the proxy handler.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	handler, err := proxy.New(cfg, logger)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: logger}
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		handler.ServeHTTP(w, r)
	})
	s.http = &http.Server{
		Handler:           root,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		IdleTimeout:       cfg.Server.IdleTimeout.Duration(),
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		// No WriteTimeout: responses are streamed and may stay open for a long time.
	}
	return s, nil
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.cfg.Server.Listen }

// Run listens until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	// "tcp4" keeps the listener IPv4-only, matching the outbound behaviour.
	listener, err := net.Listen("tcp4", s.cfg.Server.Listen)
	if err != nil {
		return err
	}
	s.log.Info("listening", "address", listener.Addr().String(), "network", "tcp4")

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(listener) }()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		s.log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Server.ShutdownTimeout.Duration())
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return nil
}

// Describe logs the configured mapping so an operator can see what is served.
// Credentials are never logged.
func (s *Server) Describe() {
	proxyMode := "system proxy (HTTP_PROXY, HTTPS_PROXY, NO_PROXY)"
	if s.cfg.Proxy.URL != "" {
		proxyMode = s.cfg.Proxy.URL
	}
	s.log.Info("upstream proxy", "mode", proxyMode)
	for i := range s.cfg.Models {
		m := &s.cfg.Models[i]
		upstream, ok := s.cfg.Upstream(m.Upstream)
		if !ok {
			continue
		}
		s.log.Info("model mapping",
			"model", m.Name,
			"upstream", upstream.Name,
			"upstream_model", m.UpstreamModel,
			"base_url", upstream.BaseURL,
			"client_key", "configured",
		)
	}
}
