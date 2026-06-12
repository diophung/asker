package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // register database/sql driver "pgx"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	migrateAttempts = 10
	migrateBackoff  = 2 * time.Second
)

// runMigrations applies the embedded migrations, retrying while Postgres
// comes up (compose start order is healthcheck-gated, but a freshly healthy
// Postgres can still drop the first connection). Idempotent: an up-to-date
// schema is a no-op.
func runMigrations(ctx context.Context, databaseURL string, logger *slog.Logger) error {
	var lastErr error
	for attempt := 1; attempt <= migrateAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = applyMigrations(databaseURL, logger)
		if lastErr == nil {
			return nil
		}
		logger.Warn("migrations attempt failed",
			"attempt", attempt, "max_attempts", migrateAttempts, "error", lastErr)
		if attempt < migrateAttempts {
			select {
			case <-time.After(migrateBackoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("migrations failed after %d attempts: %w", migrateAttempts, lastErr)
}

func applyMigrations(databaseURL string, logger *slog.Logger) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = db.Close() }()

	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("init migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("init migrate: %w", err)
	}

	err = m.Up()
	switch {
	case errors.Is(err, migrate.ErrNoChange):
		logger.Info("migrations: schema already up to date")
	case err != nil:
		return fmt.Errorf("apply migrations: %w", err)
	default:
		version, dirty, vErr := m.Version()
		if vErr != nil {
			logger.Info("migrations applied")
		} else {
			logger.Info("migrations applied", "version", version, "dirty", dirty)
		}
	}
	return nil
}
