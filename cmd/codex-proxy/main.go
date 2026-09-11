// Command codex-proxy forwards Codex Responses API requests to configured
// upstream models, rewriting only the model name and the API key.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/haojiezhang/codex-proxy/internal/config"
	"github.com/haojiezhang/codex-proxy/internal/server"
	"github.com/haojiezhang/codex-proxy/internal/version"
)

const usage = `codex-proxy - Responses API forwarder

Usage:
  codex-proxy [command] [flags]

Commands:
  run        run the proxy (default, also the Windows service entry point)
  validate   check the configuration file and exit
  version    print the build information
  help       show this message

Flags:
  -config PATH   configuration file (default: $CODEX_PROXY_CONFIG, ./config.yaml,
                 or the platform default location)
  -version       print the build information and exit
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// bootstrapLogger is used before the configuration (and with it the log level,
// format and destination) is known: everything goes to stderr.
func bootstrapLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func run(args []string) int {
	logger := bootstrapLogger()

	command := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}

	flags := flag.NewFlagSet("codex-proxy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "path to the configuration file")
	showVersion := flags.Bool("version", false, "print the build information and exit")
	// Help is the output of the command line itself rather than a log record,
	// so it is written to the flag set's stream. Everything diagnostic goes
	// through the logger.
	flags.Usage = func() {
		fmt.Fprint(flags.Output(), usage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion || command == "version" {
		// Not a log record: the version is the output of the command.
		fmt.Println("codex-proxy " + version.String())
		return 0
	}

	switch command {
	case "run", "serve", "service":
		return runServer(*configPath)
	case "validate", "check":
		return validateConfig(*configPath)
	case "help":
		flags.Usage()
		return 0
	default:
		logger.Error("unknown command", "command", command)
		flags.Usage()
		return 2
	}
}

func validateConfig(configPath string) int {
	logger := bootstrapLogger()
	path, err := config.DiscoverPath(configPath)
	if err != nil {
		logger.Error("cannot find the configuration", "error", err.Error())
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		logger.Error("invalid configuration", "config", path, "error", err.Error())
		return 1
	}
	for _, warning := range cfg.Warnings() {
		logger.Warn("configuration warning", "config", path, "detail", warning)
	}
	logger.Info("configuration is valid", "config", path,
		"upstreams", len(cfg.Upstreams), "models", len(cfg.Models))
	return 0
}

// serve loads the configuration and runs the proxy until ctx is cancelled.
// fallback receives the logs when the configuration does not name a log file.
func serve(ctx context.Context, configPath string, fallback io.Writer) int {
	logger := bootstrapLogger()

	path, err := config.DiscoverPath(configPath)
	if err != nil {
		logger.Error("cannot find the configuration", "error", err.Error())
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		logger.Error("invalid configuration", "config", path, "error", err.Error())
		return 1
	}
	configured, closer, err := newLogger(cfg, fallback)
	if err != nil {
		logger.Error("cannot set up logging", "error", err.Error())
		return 1
	}
	if closer != nil {
		defer closer.Close()
	}
	logger = configured

	logger.Info("starting codex-proxy", "version", version.String(), "config", path)
	for _, warning := range cfg.Warnings() {
		logger.Warn("configuration warning", "detail", warning)
	}

	srv, err := server.New(cfg, logger)
	if err != nil {
		logger.Error("cannot start", "error", err.Error())
		return 1
	}
	srv.Describe()
	if err := srv.Run(ctx); err != nil {
		logger.Error("server stopped with an error", "error", err.Error())
		return 1
	}
	logger.Info("stopped")
	return 0
}

// newLogger builds the logger described by the log section of the config.
func newLogger(cfg *config.Config, fallback io.Writer) (*slog.Logger, io.Closer, error) {
	options := &slog.HandlerOptions{Level: parseLevel(cfg.Log.Level)}

	var writer io.Writer = os.Stdout
	var closer io.Closer
	if path := strings.TrimSpace(cfg.Log.File); path != "" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, nil, fmt.Errorf("create log directory %s: %w", dir, err)
			}
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, nil, fmt.Errorf("open log file %s: %w", path, err)
		}
		writer = file
		closer = file
	} else if fallback != nil {
		writer = fallback
	}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Log.Format)) {
	case "json":
		handler = slog.NewJSONHandler(writer, options)
	default:
		handler = slog.NewTextHandler(writer, options)
	}
	return slog.New(handler), closer, nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
