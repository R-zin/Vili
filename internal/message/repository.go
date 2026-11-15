package message

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// foreignKeyViolation is the Postgres SQLSTATE for FK violations.
const foreignKeyViolation = "23503"

// msgColumns is the shared projection for scanning a Message joined to its
// author. mentions is read as JSON because the stdlib driver would otherwise
// hand back a text[] as an unparsed string.
const msgColumns = `m.id, m.room_id, m.user_id, u.username, m.content, m.type, m.created_at,
	m.edited_at, (m.deleted_at IS NOT NULL), m.reply_to, to_jsonb(m.mentions)`

// PostgresRepository is the production Repository backed by *sql.DB.
type PostgresRepository struct {
	db *sql.DB
}

// NewPostgresRepository builds a PostgresRepository.
func NewPostgresRepository(db *sql.DB) *PostgresRepository {
	return &PostgresRepository{db: db}
}

type rowScanner interface{ Scan(dest ...any) error }

// scanMessage scans msgColumns plus any extra destinations.
func scanMessage(s rowScanner, m *Message, extra ...any) error {
	var typ string
	var mentions []byte
	dest := append([]any{&m.ID, &m.RoomID, &m.UserID, &m.Username, &m.Content, &typ, &m.CreatedAt,
		&m.EditedAt, &m.Deleted, &m.ReplyTo, &mentions}, extra...)
	if err := s.Scan(dest...); err != nil {
		return err
	}
	m.Type = MessageType(typ)
	m.Mentions = []string{}
	if len(mentions) > 0 {
		if err := json.Unmarshal(mentions, &m.Mentions); err != nil {
			return fmt.Errorf("decode mentions: %w", err)
		}
	}
	return nil
}

// ResolveMentions narrows candidate usernames to those of actual room members,
// preserving the candidates' order. Unknown names are silently dropped, so
// "@nobody" never becomes a mention.
func (r *PostgresRepository) ResolveMentions(ctx context.Context, roomID uuid.UUID, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return []string{}, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT u.username FROM room_members rm JOIN users u ON u.id = rm.user_id
		WHERE rm.room_id = $1 AND u.username = ANY ($2)`, roomID, candidates)
	if err != nil {
		return nil, fmt.Errorf("message: resolve mentions: %w", err)
	}
	defer rows.Close()
	valid := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("message: resolve mentions scan: %w", err)
		}
		valid[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(valid))
	for _, c := range candidates {
		if valid[c] {
			out = append(out, c)
			delete(valid, c) // dedupe
		}
	}
	return out, nil
}

// Create inserts a message, generating the id and applying type/timestamp
// defaults, then returns the stored row including the author username joined
// from users in the same statement (so a broadcast right after Create carries
// the author name).
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

	if msg.ReplyTo != nil {
		var ok bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM messages WHERE id = $1 AND room_id = $2)`,
			*msg.ReplyTo, msg.RoomID).Scan(&ok); err != nil {
			return fmt.Errorf("message: check reply target: %w", err)
		}
		if !ok {
			return ErrReplyNotFound
		}
	}
	mentions, err := r.ResolveMentions(ctx, msg.RoomID, msg.Mentions)
	if err != nil {
		return err
	}
	msg.Mentions = mentions

	err = r.db.QueryRowContext(ctx, `
		WITH ins AS (
			INSERT INTO messages (id, room_id, user_id, content, type, created_at, reply_to, mentions)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING user_id, created_at
		)
		SELECT i.created_at, u.username
		FROM ins i
		JOIN users u ON u.id = i.user_id`,
		msg.ID, msg.RoomID, msg.UserID, msg.Content, string(msg.Type), msg.CreatedAt, msg.ReplyTo, msg.Mentions,
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
// and limit. Deleted messages appear as tombstones.
func (r *PostgresRepository) ListByRoom(ctx context.Context, roomID uuid.UUID, limit int, before *time.Time) ([]Message, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM rooms WHERE id = $1)`, roomID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("message: check room: %w", err)
	}
	if !exists {
		return nil, ErrRoomNotFound
	}

	const base = `
		SELECT ` + msgColumns + `
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
		if err := scanMessage(rows, &m); err != nil {
			return nil, fmt.Errorf("message: scan: %w", err)
		}
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

// IsMember reports whether userID has a room_members row for roomID.
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

// MemberRole returns the member's role or ErrNotMember.
func (r *PostgresRepository) MemberRole(ctx context.Context, roomID, userID uuid.UUID) (string, error) {
	var role string
	err := r.db.QueryRowContext(ctx,
		`SELECT role FROM room_members WHERE room_id = $1 AND user_id = $2`, roomID, userID).Scan(&role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotMember
		}
		return "", fmt.Errorf("message: member role: %w", err)
	}
	return role, nil
}

// ByID returns a message (tombstones included) or ErrMessageNotFound.
func (r *PostgresRepository) ByID(ctx context.Context, id uuid.UUID) (Message, error) {
	var m Message
	err := scanMessage(r.db.QueryRowContext(ctx, `
		SELECT `+msgColumns+` FROM messages m JOIN users u ON u.id = m.user_id WHERE m.id = $1`, id), &m)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Message{}, ErrMessageNotFound
		}
		return Message{}, fmt.Errorf("message: by id: %w", err)
	}
	return m, nil
}

// Edit updates a live message and returns the stored result.
func (r *PostgresRepository) Edit(ctx context.Context, id uuid.UUID, content string, candidates []string) (Message, error) {
	cur, err := r.ByID(ctx, id)
	if err != nil {
		return Message{}, err
	}
	mentions, err := r.ResolveMentions(ctx, cur.RoomID, candidates)
	if err != nil {
		return Message{}, err
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE messages SET content = $2, mentions = $3, edited_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id, content, mentions)
	if err != nil {
		return Message{}, fmt.Errorf("message: edit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Message{}, ErrMessageNotFound
	}
	return r.ByID(ctx, id)
}

// Delete tombstones a live message, erasing its content.
func (r *PostgresRepository) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE messages SET content = '', mentions = '{}', edited_at = NULL, deleted_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("message: delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrMessageNotFound
	}
	return nil
}

// Search runs a websearch-syntax full-text query over live messages in rooms
// the user belongs to.
func (r *PostgresRepository) Search(ctx context.Context, userID uuid.UUID, query string, roomID *uuid.UUID, limit int) ([]Message, error) {
	var room any
	if roomID != nil {
		room = roomID.String()
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+msgColumns+`, rm_room.name
		FROM messages m
		JOIN users u ON u.id = m.user_id
		JOIN rooms rm_room ON rm_room.id = m.room_id
		JOIN room_members me ON me.room_id = m.room_id AND me.user_id = $1
		WHERE m.deleted_at IS NULL
		  AND m.tsv @@ websearch_to_tsquery('simple', $2)
		  AND ($3::uuid IS NULL OR m.room_id = $3::uuid)
		ORDER BY m.created_at DESC
		LIMIT $4`, userID, query, room, limit)
	if err != nil {
		return nil, fmt.Errorf("message: search: %w", err)
	}
	defer rows.Close()
	out := make([]Message, 0)
	for rows.Next() {
		var m Message
		if err := scanMessage(rows, &m, &m.RoomName); err != nil {
			return nil, fmt.Errorf("message: search scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
