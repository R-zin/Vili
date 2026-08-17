// Package room is the room feature: it owns the Room and RoomMember types,
// the room repository (against *sql.DB via pgx stdlib), and the HTTP handlers
// for creating, listing, joining, and leaving rooms. It is self-contained;
// no other feature package imports it.
package room

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Room is a chat room.
type Room struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MemberRole is a member's role within a room.
type MemberRole string

// Supported member roles (mirrors the CHECK constraint in the schema).
const (
	RoleOwner  MemberRole = "owner"
	RoleAdmin  MemberRole = "admin"
	RoleMember MemberRole = "member"
)

// RoomMember is a user's membership in a room.
type RoomMember struct {
	RoomID   uuid.UUID  `json:"room_id"`
	UserID   uuid.UUID  `json:"user_id"`
	Role     MemberRole `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
}

// Sentinel errors mapped to HTTP responses by the handler.
var (
	ErrNotFound      = errors.New("room not found")
	ErrNameTaken     = errors.New("room name already taken")
	ErrAlreadyMember = errors.New("user is already a member")
	ErrNotMember     = errors.New("user is not a member")
)

// Repository is the narrow persistence interface the Handler depends on. It
// is satisfied by PostgresRepository in production and in-memory fakes in
// tests.
type Repository interface {
	// Create inserts a room owned by ownerID, returning ErrNameTaken on a
	// duplicate name.
	Create(ctx context.Context, ownerID uuid.UUID, name, description string) (Room, error)
	// List returns all rooms, newest first.
	List(ctx context.Context) ([]Room, error)
	// ByID returns the room or ErrNotFound.
	ByID(ctx context.Context, id uuid.UUID) (Room, error)
	// AddMember adds userID to the room with the given role, returning
	// ErrNotFound for an unknown room or ErrAlreadyMember if already joined.
	AddMember(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error
	// RemoveMember removes userID from the room, returning ErrNotFound for an
	// unknown room or ErrNotMember if not joined.
	RemoveMember(ctx context.Context, roomID, userID uuid.UUID) error
}
