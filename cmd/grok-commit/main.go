package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/ddhjy/grok-commit/internal/commit"
)

var version = "dev"

func main() {
	if version == "dev" {
		if info, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(info.Main.Version, "v") {
			version = strings.TrimPrefix(info.Main.Version, "v")
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := commit.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version); err != nil {
		os.Exit(commit.Report(os.Stderr, err))
	}
	commit.MaybeAutoUpdate(version, os.Args[1:])
}
