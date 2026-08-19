package message

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// foreignKeyViolation is the Postgres SQLSTATE for FK violations.
const foreignKeyViolation = "23503"

// PostgresRepository is the production Repository backed by *sql.DB.
type PostgresRepository struct {
	db *sql.DB
}

// NewPostgresRepository builds a PostgresRepository.
func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

// Create inserts a message, generating the id and applying type/timestamp
// defaults, then returns the stored row including the author username joined
// from users in the same statement (so a broadcast right after Create carries
// the author name). A reference to a missing room maps to ErrRoomNotFound.
func (r *PostgresRepository) Create(ctx context.Context, msg *Message) error {
	if msg.ID == uuid.Nil {
		msg.ID = uuid.New()
	}
	if msg.Type == "" {
		msg.Type = TypeText
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	err := r.db.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO messages (id, room_id, user_id, content, type, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING user_id, created_at
		)
		SELECT i.created_at, u.username
		FROM ins i
		JOIN users u ON u.id = i.user_id`,
		msg.ID, msg.RoomID, msg.UserID, msg.Content, string(msg.Type), msg.CreatedAt,
	).Scan(&msg.CreatedAt, &msg.Username)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == foreignKeyViolation {
			return ErrRoomNotFound
		}
		return fmt.Errorf("message: create: %w", err)
	}
	return nil
}

// ListByRoom returns room history, oldest first, applying the before cursor
// and limit. Username is produced by joining users; the query uses a
// parameterized room id, created_at cursor, and limit.
func (r *PostgresRepository) ListByRoom(ctx context.Context, roomID uuid.UUID, limit int, before *time.Time) ([]Message, error) {
	// Confirm the room exists so callers can distinguish 404 from an empty
	// history.
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms WHERE id = $1)`, roomID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("message: check room: %w", err)
	}
	if !exists {
		return nil, ErrRoomNotFound
	}

	// Fetch newest-first up to limit (optionally bounded by the before
	// cursor), then reverse to chronological order for the response.
	const base = `
		SELECT m.id, m.room_id, m.user_id, u.username, m.content, m.type, m.created_at
		FROM messages m
		JOIN users u ON u.id = m.user_id
		WHERE m.room_id = $1 `

	var query string
	var args []any
	if before != nil && !before.IsZero() {
		query = base + `AND m.created_at < $2 ORDER BY m.created_at DESC LIMIT $3`
		args = []any{roomID, *before, limit}
	} else {
		query = base + `ORDER BY m.created_at DESC LIMIT $2`
		args = []any{roomID, limit}
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("message: list: %w", err)
	}
	defer rows.Close()

	messages := make([]Message, 0)
	for rows.Next() {
		var m Message
		var typ string
		if err := rows.Scan(&m.ID, &m.RoomID, &m.UserID, &m.Username, &m.Content, &typ, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("message: scan: %w", err)
		}
		m.Type = MessageType(typ)
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("message: rows: %w", err)
	}

	// Reverse to ascending chronological order (oldest to newest).
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

// IsMember reports whether userID rows exist in room_members for roomID. The
// package is self-contained, so membership is checked here directly rather
// than by importing the room feature.
func (r *PostgresRepository) IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error) {
	var member bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM room_members WHERE room_id = $1 AND user_id = $2)`,
		roomID, userID,
	).Scan(&member); err != nil {
		return false, fmt.Errorf("message: is member: %w", err)
	}
	return member, nil
}
