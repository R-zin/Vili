package ws

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Presence liveness: instances heartbeat every PresenceHeartbeat; a peer that
// has been silent for presenceStaleAfter is treated as gone and its presence
// rows are ignored (the janitor deletes them later).
const (
	PresenceHeartbeat  = 10 * time.Second
	presenceStaleAfter = 35 * time.Second
)

// PGPresence is a PresenceStore shared by all instances through Postgres.
type PGPresence struct {
	db       *sql.DB
	instance string
	// snapshot rebuilds this instance's rows if they were lost (e.g. the
	// janitor removed the instance during a long stall). Usually Hub.LocalPresence.
	snapshot func() []PresenceEntry
}

// NewPGPresence builds a store for instance. snapshot may be nil.
func NewPGPresence(db *sql.DB, instance string, snapshot func() []PresenceEntry) *PGPresence {
	return &PGPresence{db: db, instance: instance, snapshot: snapshot}
}

// Register announces this instance, clearing any rows left by a previous
// process that used the same id.
func (p *PGPresence) Register(ctx context.Context) error {
	if _, err := p.db.ExecContext(ctx, `DELETE FROM ws_instances WHERE id = $1`, p.instance); err != nil {
		return fmt.Errorf("presence: register: %w", err)
	}
	if _, err := p.db.ExecContext(ctx, `INSERT INTO ws_instances (id) VALUES ($1)`, p.instance); err != nil {
		return fmt.Errorf("presence: register: %w", err)
	}
	return nil
}

// Deregister removes this instance and (by cascade) its presence rows, so
// peers stop listing its users immediately on a clean shutdown.
func (p *PGPresence) Deregister(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM ws_instances WHERE id = $1`, p.instance)
	return err
}

// Run heartbeats until ctx is cancelled. If the instance row vanished, it is
// re-created and this instance's presence restored from the snapshot.
func (p *PGPresence) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.heartbeat(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("presence heartbeat failed", "error", err)
			}
		}
	}
}

func (p *PGPresence) heartbeat(ctx context.Context) error {
	res, err := p.db.ExecContext(ctx, `UPDATE ws_instances SET heartbeat_at = now() WHERE id = $1`, p.instance)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	slog.Warn("presence: instance row was missing; re-registering", "instance", p.instance)
	if err := p.Register(ctx); err != nil {
		return err
	}
	if p.snapshot == nil {
		return nil
	}
	for _, e := range p.snapshot() {
		if _, err := p.db.ExecContext(ctx, `
			INSERT INTO ws_presence (instance_id, room_id, user_id, username, conns) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (instance_id, room_id, user_id) DO UPDATE SET conns = EXCLUDED.conns`,
			p.instance, e.Room, e.User, e.Username, e.Conns); err != nil {
			return err
		}
	}
	return nil
}

// Add counts one more connection of userID to roomID on this instance.
func (p *PGPresence) Add(ctx context.Context, roomID, userID uuid.UUID, username string) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO ws_presence (instance_id, room_id, user_id, username, conns) VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (instance_id, room_id, user_id) DO UPDATE SET conns = ws_presence.conns + 1, username = EXCLUDED.username`,
		p.instance, roomID, userID, username)
	if err != nil {
		return fmt.Errorf("presence: add: %w", err)
	}
	return nil
}

// Remove counts one connection fewer, deleting the row when none remain. The
// decrement and the delete are one statement each and the delete only matches
// a row at zero, so concurrent opens/closes can't lose or resurrect a count.
func (p *PGPresence) Remove(ctx context.Context, roomID, userID uuid.UUID) error {
	if _, err := p.db.ExecContext(ctx, `
		UPDATE ws_presence SET conns = conns - 1
		WHERE instance_id = $1 AND room_id = $2 AND user_id = $3 AND conns > 0`,
		p.instance, roomID, userID); err != nil {
		return fmt.Errorf("presence: remove: %w", err)
	}
	if _, err := p.db.ExecContext(ctx, `
		DELETE FROM ws_presence WHERE instance_id = $1 AND room_id = $2 AND user_id = $3 AND conns = 0`,
		p.instance, roomID, userID); err != nil {
		return fmt.Errorf("presence: remove: %w", err)
	}
	return nil
}

// Online returns the distinct usernames connected to roomID on any live
// instance, sorted.
func (p *PGPresence) Online(ctx context.Context, roomID uuid.UUID) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT DISTINCT p.username
		FROM ws_presence p JOIN ws_instances i ON i.id = p.instance_id
		WHERE p.room_id = $1 AND p.conns > 0 AND i.heartbeat_at > now() - $2::interval
		ORDER BY p.username`, roomID, fmt.Sprintf("%d seconds", int(presenceStaleAfter.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("presence: online: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
