// Package message is the message feature: it owns the Message type, the
// message repository (against *sql.DB via pgx stdlib), and the HTTP handler
// for listing a room's history. It is self-contained; the username shown for
// each message comes from this package's own SQL JOIN to users, not from
// importing the user feature.
package message

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// MessageType classifies a message's content.
type MessageType string

// Supported message types (mirrors the CHECK constraint in the schema).
const (
	TypeText   MessageType = "text"
	TypeDiff   MessageType = "diff"
	TypeCode   MessageType = "code"
	TypeLog    MessageType = "log"
	TypeCommit MessageType = "commit"
)

// Message is a single chat message. Username is populated by joining users at
// read time; it is not a stored column.
type Message struct {
	ID        uuid.UUID   `json:"id"`
	RoomID    uuid.UUID   `json:"room_id"`
	UserID    uuid.UUID   `json:"user_id"`
	Username  string      `json:"username"`
	Content   string      `json:"content"`
	Type      MessageType `json:"type"`
	CreatedAt time.Time   `json:"created_at"`
}

// Sentinel errors mapped to HTTP responses by the handler.
var (
	ErrRoomNotFound = errors.New("room not found")
	ErrNotMember    = errors.New("user is not a member")
)

// Repository is the narrow persistence interface the Handler depends on. It
// is satisfied by PostgresRepository in production and in-memory fakes in
// tests.
type Repository interface {
	// Create stores a message, applying defaults (id, type, created_at) and
	// returning the stored message. An unknown room maps to ErrRoomNotFound.
	Create(ctx context.Context, msg *Message) error
	// ListByRoom returns up to limit messages in the room, optionally only
	// those created before the cursor, oldest-first. An unknown room maps to
	// ErrRoomNotFound.
	ListByRoom(ctx context.Context, roomID uuid.UUID, limit int, before *time.Time) ([]Message, error)
	// IsMember reports whether userID belongs to the room identified by
	// roomID. The handler uses it to gate reading history and posting.
	IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error)
}
