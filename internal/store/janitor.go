package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Retention windows. Consumed/expired refresh tokens are kept briefly past
// expiry so a replayed stolen token is still recognised as reuse (and its
// family revoked) rather than looking merely unknown.
const (
	refreshTokenGrace = 24 * time.Hour
	inviteGrace       = 7 * 24 * time.Hour
)

// Purge deletes expired refresh tokens and expired invites past their grace
// periods (returning how many of each were removed), stale cluster instances
// and old bus outbox rows.
func Purge(ctx context.Context, db *sql.DB) (tokens, invites int64, err error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM refresh_tokens WHERE expires_at < now() - $1::interval`, fmt.Sprintf("%d seconds", int(refreshTokenGrace.Seconds())))
	if err != nil {
		return 0, 0, fmt.Errorf("store: purge refresh tokens: %w", err)
	}
	tokens, _ = res.RowsAffected()

	res, err = db.ExecContext(ctx,
		`DELETE FROM room_invites WHERE expires_at < now() - $1::interval`, fmt.Sprintf("%d seconds", int(inviteGrace.Seconds())))
	if err != nil {
		return tokens, 0, fmt.Errorf("store: purge invites: %w", err)
	}
	invites, _ = res.RowsAffected()

	// Cluster housekeeping: instances that stopped heartbeating long ago
	// (crashed; their presence rows cascade) and delivered bus payloads.
	if _, err := db.ExecContext(ctx,
		`DELETE FROM ws_instances WHERE heartbeat_at < now() - interval '5 minutes'`); err != nil {
		return tokens, invites, fmt.Errorf("store: purge stale instances: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM bus_outbox WHERE created_at < now() - interval '10 minutes'`); err != nil {
		return tokens, invites, fmt.Errorf("store: purge bus outbox: %w", err)
	}
	return tokens, invites, nil
}

// RunJanitor purges expired rows once at start and then every interval until
// ctx is cancelled. Failures are logged and retried on the next tick; the
// janitor never takes the server down.
func RunJanitor(ctx context.Context, db *sql.DB, interval time.Duration) {
	run := func() {
		t, i, err := Purge(ctx, db)
		switch {
		case err != nil && ctx.Err() == nil:
			slog.Error("janitor purge failed", "error", err)
		case t+i > 0:
			slog.Info("janitor purged expired rows", "refresh_tokens", t, "invites", i)
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
