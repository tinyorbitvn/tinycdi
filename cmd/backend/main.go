// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

// Command backend is the merged tinycdi control-plane binary: the public
// API, the session gateway and the in-process broker on four listeners
// (app, session, internal mTLS, metrics). See internal/backend for the
// flag surface and wiring.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tinyorbitvn/tinycdi/internal/backend"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One-shot subcommand: deterministic post-restore cleanup
	// (docs/runbooks/disaster-recovery.md). Runnable as a Job — the DB
	// steps need no ServiceAccount token.
	if len(os.Args) > 1 && os.Args[1] == "post-restore" {
		os.Exit(backend.PostRestoreMain(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr, log))
	}

	cfg, err := backend.ParseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}

	b, err := backend.New(ctx, cfg, log)
	if err != nil {
		log.Error("init", "err", err)
		os.Exit(1)
	}
	if err := b.Run(ctx); err != nil {
		log.Error("run", "err", err)
		os.Exit(1)
	}
}
