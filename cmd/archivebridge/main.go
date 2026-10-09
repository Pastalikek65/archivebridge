package main

import (
	"context"
	"os"
	"os/signal"
)

// These variables can be set by packaging builds with -ldflags.
// The source tree targets the v1 release line; publication remains a separate
// qualification decision. Packaging can override this with -ldflags.
var version = "1.0.0"
var commit = "development"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	exitCode := safeExit(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(exitCode)
}
