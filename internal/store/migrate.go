package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

// migrationsFS holds the SQL migration files. Each *.up.sql file is applied
// in filename (lexicographic) order inside its own transaction.
//
//go:embed migrations/*.up.sql
var migrationsFS embed.FS

// migrateLockKey is the advisory-lock key serializing migrations across
// instances ("vili" in ASCII).
const migrateLockKey = 0x76696c69

// Migrate applies all pending *.up.sql migrations in order and reports the
// versions it applied. It is idempotent: already-applied versions are skipped
// (tracked in schema_migrations), so running it on every startup is safe.
//
// Several instances may start at once (scale-out, rolling deploys): the whole
// run holds a Postgres session advisory lock, so they migrate one at a time
// and the later ones find nothing left to do.
func Migrate(ctx context.Context, db *sql.DB) ([]string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrateLockKey); err != nil {
		return nil, fmt.Errorf("store: migration lock: %w", err)
	}
	defer func() {
		// Unlock even if ctx was cancelled; closing the connection would also
		// release it, but pooled connections are reused.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrateLockKey)
	}()
	return migrate(ctx, conn)
}

// migrate runs the migrations on conn (which holds the migration lock).
func migrate(ctx context.Context, conn *sql.Conn) ([]string, error) {
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return nil, fmt.Errorf("store: create schema_migrations: %w", err)
	}

	names, err := migrationNames()
	if err != nil {
		return nil, err
	}

	applied := make(map[string]bool, len(names))
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: iterate schema_migrations: %w", err)
	}
	rows.Close()

	var ran []string
	for _, name := range names {
		version := strings.TrimSuffix(name, ".up.sql")
		if applied[version] {
			continue
		}
		if err := applyOne(ctx, conn, name, version); err != nil {
			return nil, err
		}
		ran = append(ran, version)
	}
	return ran, nil
}

// migrationNames returns the embedded *.up.sql file names in sorted order.
func migrationNames() ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("store: no embedded migrations found")
	}
	return names, nil
}

// applyOne runs a single migration file in a transaction and records it.
func applyOne(ctx context.Context, conn *sql.Conn, name, version string) error {
	body, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("store: read migration %s: %w", name, err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", version, err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return fmt.Errorf("store: record migration %s: %w", version, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", version, err)
	}
	return nil
}
