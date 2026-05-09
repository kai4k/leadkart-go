// Package main is the LeadKart goose migration runner.
//
// Reads migration files from a directory (default `./migrations`) supplied
// via flag or env var. The Dockerfile copies migrations/ into the image
// alongside the binary; local dev runs from the repo root.
//
// Embedding migrations into the binary is a future optimisation (v0.2+) —
// requires either moving migrations/ under cmd/migrate/ or building an
// internal/migrations package whose //go:embed pattern can reach the
// migrations/ directory at the repo root.
//
// Usage:
//
//	migrate up                    # apply all pending
//	migrate up-to <version>       # apply up to specific version
//	migrate down                  # roll back one
//	migrate status                # show applied vs pending
//	migrate version               # current version
//	migrate validate              # parse-check all migration files
//
// Environment:
//
//	DATABASE_URL       postgresql connection string (required)
//	MIGRATIONS_DIR     path to migrations directory (default: ./migrations)
//	GOOSE_TABLE        history table name (default: goose_db_version)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/pressly/goose/v3"

	// register pgx with database/sql
	_ "github.com/jackc/pgx/v5/stdlib"

	// Blank-import the platform Go-shaped migrations so their init()
	// blocks register with goose's global registry before
	// goose.RunContext collects migrations to apply. SQL files in
	// `migrations/` are picked up alongside; goose merges by version.
	_ "github.com/leadkart/leadkart-go/internal/platform/migrations"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if len(os.Args) < 2 {
		return errors.New("usage: migrate <up|up-to|down|status|version|validate> [args...]")
	}
	command := os.Args[1]

	// Read the canonical project-namespaced env var first; fall back
	// to bare DATABASE_URL for one-off scripts + legacy CI compat. The
	// fall-back will be dropped in v0.3 when every binary is on the
	// LEADKART_* namespace per CLAUDE.md "Env naming" doctrine.
	dsn := os.Getenv("LEADKART_POSTGRES__DSN")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return errors.New("LEADKART_POSTGRES__DSN env var required (DATABASE_URL also accepted)")
	}

	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = "./migrations"
	}

	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			logger.Warn("db close", "err", cerr)
		}
	}()

	if tbl := os.Getenv("GOOSE_TABLE"); tbl != "" {
		goose.SetTableName(tbl)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	logger.InfoContext(ctx, "running migration command",
		"command", command,
		"migrations_dir", migrationsDir,
		"dsn_host", maskDSN(dsn),
	)

	if err := goose.RunContext(ctx, command, db, migrationsDir, os.Args[2:]...); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}
	return nil
}

// maskDSN returns the host:port + database portion of a DSN for safe
// logging — strips userinfo (username + password) entirely. Uses
// net/url.Parse for correctness against passwords containing `@` or
// `:` (which the previous IndexByte('@') heuristic mishandled, e.g.
// "postgres://u:p@ssword@host/db" would log "ssword@host/db").
func maskDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "<unknown>"
	}
	host := u.Host
	if u.Path != "" {
		host += u.Path
	}
	return host
}
