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
// read time; it is not a stored column. A deleted message is kept as a
// tombstone (Deleted true, Content empty) so replies to it still make sense.
type Message struct {
	ID        uuid.UUID   `json:"id"`
	RoomID    uuid.UUID   `json:"room_id"`
	UserID    uuid.UUID   `json:"user_id"`
	Username  string      `json:"username"`
	Content   string      `json:"content"`
	Type      MessageType `json:"type"`
	CreatedAt time.Time   `json:"created_at"`
	EditedAt  *time.Time  `json:"edited_at,omitempty"`
	Deleted   bool        `json:"deleted,omitempty"`
	ReplyTo   *uuid.UUID  `json:"reply_to,omitempty"`
	// Mentions are the usernames of room members @mentioned in Content.
	// On Create/Edit the repository narrows the candidates to real members.
	Mentions []string `json:"mentions"`
	// RoomName is set on search results only.
	RoomName string `json:"room_name,omitempty"`
}

// Sentinel errors mapped to HTTP responses by the handler.
var (
	ErrRoomNotFound    = errors.New("room not found")
	ErrNotMember       = errors.New("user is not a member")
	ErrMessageNotFound = errors.New("message not found")
	ErrReplyNotFound   = errors.New("reply target not found in this room")
)

// Repository is the narrow persistence interface the Handler depends on. It
// is satisfied by PostgresRepository in production and in-memory fakes in
// tests.
type Repository interface {
	// Create stores a message, applying defaults (id, type, created_at) and
	// returning the stored message. msg.Mentions are candidates and are
	// narrowed to actual room members. An unknown room maps to ErrRoomNotFound
	// and a reply target outside the room to ErrReplyNotFound.
	Create(ctx context.Context, msg *Message) error
	// ListByRoom returns up to limit messages in the room, optionally only
	// those created before the cursor, oldest-first. An unknown room maps to
	// ErrRoomNotFound.
	ListByRoom(ctx context.Context, roomID uuid.UUID, limit int, before *time.Time) ([]Message, error)
	// IsMember reports whether userID belongs to the room identified by
	// roomID. The handler uses it to gate reading history and posting.
	IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error)
	// MemberRole returns userID's role in the room ("owner", "admin",
	// "member") or ErrNotMember.
	MemberRole(ctx context.Context, roomID, userID uuid.UUID) (string, error)
	// ByID returns a message (including tombstones) or ErrMessageNotFound.
	ByID(ctx context.Context, id uuid.UUID) (Message, error)
	// Edit replaces a live message's content and mentions and stamps edited_at.
	// Deleted or unknown messages yield ErrMessageNotFound.
	Edit(ctx context.Context, id uuid.UUID, content string, mentionCandidates []string) (Message, error)
	// Delete tombstones a live message (content is erased). Already-deleted or
	// unknown messages yield ErrMessageNotFound.
	Delete(ctx context.Context, id uuid.UUID) error
	// Search runs a full-text query over live messages in rooms userID belongs
	// to, newest first, optionally restricted to one room.
	Search(ctx context.Context, userID uuid.UUID, query string, roomID *uuid.UUID, limit int) ([]Message, error)
}
