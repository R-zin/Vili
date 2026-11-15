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

// Room is a chat room. Private rooms are only visible to their members and
// are joined by invite code; a direct-message room is a private two-person room.
type Room struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsPrivate   bool      `json:"is_private"`
	IsDM        bool      `json:"is_dm"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Summary is a Room as seen by one user in a listing: the caller's role (empty
// when not a member), the member count, unread counters, and for DMs the other
// participant's username.
type Summary struct {
	Room
	Role           MemberRole `json:"role,omitempty"`
	MemberCount    int        `json:"member_count"`
	Unread         int        `json:"unread"`
	UnreadMentions int        `json:"unread_mentions"`
	Peer           string     `json:"peer,omitempty"`
}

// MemberRole is a member's role within a room.
type MemberRole string

// Supported member roles (mirrors the CHECK constraint in the schema).
const (
	RoleOwner  MemberRole = "owner"
	RoleAdmin  MemberRole = "admin"
	RoleMember MemberRole = "member"
)

// rank orders roles for permission checks; unknown roles rank lowest.
func (r MemberRole) rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	}
	return 0
}

// RoomMember is a user's membership in a room.
type RoomMember struct {
	RoomID   uuid.UUID  `json:"room_id"`
	UserID   uuid.UUID  `json:"user_id"`
	Role     MemberRole `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
}

// Member is a room member with their username, for member listings.
type Member struct {
	UserID   uuid.UUID  `json:"user_id"`
	Username string     `json:"username"`
	Role     MemberRole `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
}

// Invite describes an invite code (never the code itself, which is shown once
// at creation and stored only as a digest).
type Invite struct {
	ID        uuid.UUID  `json:"id"`
	RoomID    uuid.UUID  `json:"room_id"`
	CreatedBy uuid.UUID  `json:"created_by"`
	ExpiresAt time.Time  `json:"expires_at"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// ListFilter narrows a room listing.
type ListFilter struct {
	// Query is a case-insensitive substring match on the room name.
	Query string
	// MineOnly restricts the listing to rooms the user belongs to.
	MineOnly bool
	// Limit caps the result size (the handler always sets it).
	Limit int
}

// UpdateParams are the optional fields of a room update; nil means unchanged.
type UpdateParams struct {
	Name        *string
	Description *string
	IsPrivate   *bool
}

// Sentinel errors mapped to HTTP responses by the handler.
var (
	ErrNotFound      = errors.New("room not found")
	ErrNameTaken     = errors.New("room name already taken")
	ErrAlreadyMember = errors.New("user is already a member")
	ErrNotMember     = errors.New("user is not a member")
	ErrInviteInvalid = errors.New("invite is invalid, expired, revoked or used up")
	ErrUserNotFound  = errors.New("user not found")
	ErrSelfDM        = errors.New("cannot open a direct message with yourself")
)

// Repository is the narrow persistence interface the Handler depends on. It
// is satisfied by PostgresRepository in production and in-memory fakes in
// tests.
type Repository interface {
	// Create atomically inserts a room and its owner membership, returning
	// ErrNameTaken on a duplicate name.
	Create(ctx context.Context, ownerID uuid.UUID, name, description string, private bool) (Room, error)
	// List returns the rooms visible to userID (public rooms plus private rooms
	// they belong to) with per-user counters, newest first.
	List(ctx context.Context, userID uuid.UUID, f ListFilter) ([]Summary, error)
	// ByID returns the room or ErrNotFound.
	ByID(ctx context.Context, id uuid.UUID) (Room, error)
	// Role returns userID's role in the room, or ErrNotMember (which also
	// covers an unknown room, so callers cannot probe existence).
	Role(ctx context.Context, roomID, userID uuid.UUID) (MemberRole, error)
	// AddMember adds userID to the room with the given role, returning
	// ErrNotFound for an unknown room or ErrAlreadyMember if already joined.
	AddMember(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error
	// RemoveMember removes userID from the room, returning ErrNotFound for an
	// unknown room or ErrNotMember if not joined. If the departing member was
	// the last owner, ownership passes to the longest-standing admin, else the
	// longest-standing member; an emptied room is deleted.
	RemoveMember(ctx context.Context, roomID, userID uuid.UUID) error
	// Members lists a room's members, owners first then by join time.
	Members(ctx context.Context, roomID uuid.UUID) ([]Member, error)
	// SetRole changes a member's role (ErrNotMember if absent).
	SetRole(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error
	// Update applies the non-nil fields, returning ErrNotFound / ErrNameTaken.
	Update(ctx context.Context, id uuid.UUID, p UpdateParams) (Room, error)
	// Delete removes the room and everything in it.
	Delete(ctx context.Context, id uuid.UUID) error
	// MarkRead advances the member's read cursor to now.
	MarkRead(ctx context.Context, roomID, userID uuid.UUID) error
	// CreateInvite stores an invite by code digest.
	CreateInvite(ctx context.Context, roomID, createdBy uuid.UUID, codeHash string, expiresAt time.Time, maxUses int) (Invite, error)
	// ListInvites returns a room's invites, newest first.
	ListInvites(ctx context.Context, roomID uuid.UUID) ([]Invite, error)
	// RevokeInvite revokes an invite of the room (ErrNotFound if absent).
	RevokeInvite(ctx context.Context, roomID, inviteID uuid.UUID) error
	// RedeemInvite consumes one use of the invite with codeHash and adds userID
	// as a member, atomically. A user who is already a member consumes nothing
	// and gets ErrAlreadyMember. Any unusable invite yields ErrInviteInvalid.
	RedeemInvite(ctx context.Context, codeHash string, userID uuid.UUID) (Room, error)
	// OpenDM finds or creates the direct-message room between userID and the
	// named user (ErrUserNotFound / ErrSelfDM).
	OpenDM(ctx context.Context, userID uuid.UUID, peerUsername string) (Room, error)
}
