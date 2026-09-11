//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// runServer runs in the foreground and stops on SIGINT or SIGTERM, which is how
// systemd and a terminal stop the process.
func runServer(configPath string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, configPath, nil)
}
