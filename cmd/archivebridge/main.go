package main

import (
	"context"
	"os"
	"os/signal"
)

// These variables can be set by packaging builds with -ldflags.
var version = "0.2.0"
var commit = "development"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	exitCode := safeExit(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(exitCode)
}
