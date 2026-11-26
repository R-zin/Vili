package bus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Channel is the Postgres NOTIFY channel the bus uses.
const Channel = "vili_bus"

// maxInline is the largest envelope sent inline in a NOTIFY payload; Postgres
// caps payloads at 8000 bytes. Larger messages (a 4000-character message can
// exceed it once JSON-escaped) go through the bus_outbox table and only their
// row id is notified.
const maxInline = 7000

// Reconnect backoff for the listener connection.
const (
	minBackoff = 250 * time.Millisecond
	maxBackoff = 15 * time.Second
)

// envelope is the NOTIFY payload: either an inline message or a reference to
// an outbox row. Origin lets an instance skip its own notifications (it
// already delivered them locally at publish time).
type envelope struct {
	Origin string   `json:"o"`
	Msg    *Message `json:"m,omitempty"`
	Ref    int64    `json:"ref,omitempty"`
}

// Postgres is a cluster bus over LISTEN/NOTIFY. Publish delivers locally at
// once and notifies the other instances through the pooled db; Run holds a
// dedicated listener connection (re-established with backoff) and delivers
// other instances' messages to the local subscribers.
type Postgres struct {
	handlers
	db       *sql.DB
	dsn      string
	instance string

	connected atomic.Bool
	received  atomic.Int64
	published atomic.Int64
}

// NewPostgres builds a Postgres bus. instance must be unique per process.
func NewPostgres(db *sql.DB, dsn, instance string) *Postgres {
	return &Postgres{db: db, dsn: dsn, instance: instance}
}

// Instance returns this process's instance id.
func (p *Postgres) Instance() string { return p.instance }

// Connected reports whether the listener is currently connected.
func (p *Postgres) Connected() bool { return p.connected.Load() }

// Stats returns how many messages were published and received from peers.
func (p *Postgres) Stats() (published, received int64) {
	return p.published.Load(), p.received.Load()
}

// Publish delivers m locally, then notifies the other instances. A notify
// failure is returned (and the local delivery has still happened).
func (p *Postgres) Publish(ctx context.Context, m Message) error {
	p.deliver(m)
	p.published.Add(1)

	payload, err := json.Marshal(envelope{Origin: p.instance, Msg: &m})
	if err != nil {
		return fmt.Errorf("bus: encode: %w", err)
	}
	if len(payload) > maxInline {
		body, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("bus: encode: %w", err)
		}
		var id int64
		if err := p.db.QueryRowContext(ctx,
			`INSERT INTO bus_outbox (payload) VALUES ($1) RETURNING id`, string(body)).Scan(&id); err != nil {
			return fmt.Errorf("bus: outbox insert: %w", err)
		}
		if payload, err = json.Marshal(envelope{Origin: p.instance, Ref: id}); err != nil {
			return fmt.Errorf("bus: encode: %w", err)
		}
	}
	if _, err := p.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, Channel, string(payload)); err != nil {
		return fmt.Errorf("bus: notify: %w", err)
	}
	return nil
}

// Run listens until ctx is cancelled, reconnecting with exponential backoff.
// ready, if non-nil, is closed once the first LISTEN succeeds.
func (p *Postgres) Run(ctx context.Context, ready chan<- struct{}) {
	backoff := minBackoff
	for ctx.Err() == nil {
		err := p.listen(ctx, func() {
			backoff = minBackoff
			if ready != nil {
				close(ready)
				ready = nil
			}
		})
		p.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("bus listener disconnected; peers' live updates are paused until it reconnects",
			"error", err, "retry_in", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listen runs one listener connection until it fails or ctx ends.
func (p *Postgres) listen(ctx context.Context, onReady func()) error {
	conn, err := pgx.Connect(ctx, p.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	p.connected.Store(true)
	onReady()
	slog.Info("bus listener connected", "instance", p.instance)
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		p.handle(ctx, n.Payload)
	}
}

// handle decodes one notification and delivers it unless it is our own.
func (p *Postgres) handle(ctx context.Context, payload string) {
	var env envelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		slog.Warn("bus: dropping undecodable notification", "error", err)
		return
	}
	if env.Origin == p.instance {
		return // delivered locally at publish time
	}
	msg := env.Msg
	if msg == nil && env.Ref != 0 {
		var body string
		err := p.db.QueryRowContext(ctx, `SELECT payload FROM bus_outbox WHERE id = $1`, env.Ref).Scan(&body)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				slog.Warn("bus: outbox fetch failed", "ref", env.Ref, "error", err)
			}
			return
		}
		msg = &Message{}
		if err := json.Unmarshal([]byte(body), msg); err != nil {
			slog.Warn("bus: dropping undecodable outbox row", "ref", env.Ref, "error", err)
			return
		}
	}
	if msg == nil {
		return
	}
	p.received.Add(1)
	p.deliver(*msg)
}

// NewInstanceID returns a fresh, human-readable instance id.
func NewInstanceID(hostname string) string {
	if hostname == "" {
		hostname = "vili"
	}
	return hostname + "-" + strconv.FormatInt(time.Now().Unix(), 36) + "-" + uuid.NewString()[:8]
}
