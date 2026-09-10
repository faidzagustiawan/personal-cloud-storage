// Command cloudapp is the personal cloud storage server.
//
// One binary, several subcommands:
//
//	cloudapp serve                            run the API server (default)
//	cloudapp createuser --username faidz      create the single account (spec §4.1)
//	cloudapp passwd --username faidz          reset a password from the shell
//	cloudapp reindex --from-b2 --username x   rebuild the index from storage (§7.3)
//	cloudapp version                          print the build version
//
// Configuration comes entirely from the environment; see internal/config.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"cloudapp/internal/b2"
	"cloudapp/internal/cli"
	"cloudapp/internal/config"
	"cloudapp/internal/httpapi"
	"cloudapp/internal/jobs"
	"cloudapp/internal/store"
)

// Version is set at build time: -ldflags "-X main.Version=$(git describe --tags)"
var Version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "cloudapp: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		command, args = args[0], args[1:]
	}

	if command == "version" {
		fmt.Println(Version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.DBPath, err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "serve":
		return serve(ctx, cfg, st, log)
	case "createuser":
		return cli.CreateUser(ctx, cfg, st, args)
	case "passwd":
		return cli.ChangePassword(ctx, st, args)
	case "reindex":
		return cli.Reindex(ctx, cfg, st, log, args)
	default:
		return fmt.Errorf("unknown command %q (try: serve, createuser, passwd, reindex, version)", command)
	}
}

func serve(ctx context.Context, cfg *config.Config, st *store.Store, log *slog.Logger) error {
	// A server with no accounts can never be logged into, and the reason is not
	// obvious from a 401. Say so at startup instead.
	if n, err := st.CountUsers(ctx); err == nil && n == 0 {
		log.Warn("no users exist — run: cloudapp createuser --username <name>")
	}
	if !cfg.SecureCookie {
		log.Warn("session cookie is not marked Secure — expected only for plain-HTTP local development")
	}

	storage := b2.NewService(cfg, st, log, cfg.AllowMasterUpload)
	switch status := storage.Status(ctx); status.State {
	case "unconfigured":
		log.Warn("B2 is not configured; uploads and downloads are unavailable")
	case "error":
		// Not fatal: the server should still come up so that /api/health can
		// explain what is wrong rather than the process refusing to start.
		log.Error("B2 is configured but unhealthy", "detail", status.Detail)
	default:
		log.Info("B2 ready", "upload_key_scope", status.UploadKeyScope)
	}

	// Maintenance runs alongside the server rather than from cron: it needs the
	// same configuration and the same storage client, and a job that cannot
	// start is then visible in the same log as everything else (spec §7.2).
	runner := jobs.New(st, storage, log, cfg.TempDir)
	go runner.Run(ctx)

	srv := httpapi.New(cfg, st, storage, log, Version)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}
