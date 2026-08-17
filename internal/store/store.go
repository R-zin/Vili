// Package store wires the Postgres connection and runs schema migrations.
//
// It is a small leaf package: it connects (sql.Open via the pgx stdlib
// driver), applies connection-pool settings, pings the database, and runs
// the embedded migrations. Feature packages receive the resulting *sql.DB
// and never touch driver or migration concerns themselves.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/R-zin/vili/internal/config"
)

// pingTimeout bounds the startup connectivity check.
const pingTimeout = 5 * time.Second

// Open establishes a PostgreSQL connection pool from the supplied config,
// applies pool settings, and verifies connectivity with a Ping. It returns an
// error if the database is unreachable so startup fails fast.
func Open(cfg config.Config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}

	db.SetMaxOpenConns(cfg.DB.MaxOpenConns)
	db.SetMaxIdleConns(cfg.DB.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.DB.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	return db, nil
}
