//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// windowsServiceName is the name the installer registers with the SCM.
const windowsServiceName = "codex-proxy"

// runServer runs as a Windows service when started by the Service Control
// Manager, and in the foreground otherwise.
func runServer(configPath string) int {
	isService, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-proxy:", err)
		return 1
	}
	if !isService {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return serve(ctx, configPath, nil)
	}

	handler := &windowsServiceHandler{configPath: configPath, fallback: openEventLog()}
	if err := svc.Run(windowsServiceName, handler); err != nil {
		fmt.Fprintln(os.Stderr, "codex-proxy:", err)
		return 1
	}
	return handler.exitCode
}

type windowsServiceHandler struct {
	configPath string
	fallback   io.WriteCloser
	exitCode   int
}

// Execute implements svc.Handler.
func (h *windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	defer func() {
		if h.fallback != nil {
			h.fallback.Close()
		}
	}()
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fallback io.Writer
	if h.fallback != nil {
		fallback = h.fallback
	}
	finished := make(chan int, 1)
	go func() { finished <- serve(ctx, h.configPath, fallback) }()

	status <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				h.exitCode = <-finished
				return false, 0
			}
		case code := <-finished:
			h.exitCode = code
			if code != 0 {
				return true, 1
			}
			return false, 0
		}
	}
}

// openEventLog returns a writer for the Windows Event Log when the event source
// has been registered (the installer does that). Otherwise it returns nil and
// logs go to stdout, which is only useful in console mode.
func openEventLog() io.WriteCloser {
	log, err := eventlog.Open(windowsServiceName)
	if err != nil {
		return nil
	}
	return &eventLogWriter{log: log}
}

type eventLogWriter struct {
	log *eventlog.Log
}

func (w *eventLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimRight(string(p), "\r\n")
	if message != "" {
		if err := w.log.Info(1, message); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *eventLogWriter) Close() error { return w.log.Close() }
