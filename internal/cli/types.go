// Package cli implements the Vili terminal client: an HTTP API consumer with
// subcommands for auth, rooms, and messaging, plus a simple interactive chat
// view. It speaks the backend's HTTP contract only — it defines its own view
// structs and never imports the server's internal feature packages.
package cli

import (
	"encoding/json"
	"time"
)

// User mirrors the backend's safe user JSON (user.Public).
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// loginResponse mirrors the backend's login/refresh body: an access token, the
// refresh token used to renew it, the access token's expiry, and the user.
type loginResponse struct {
	Token        string    `json:"token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	User         User      `json:"user"`
}

// Room mirrors the backend's room JSON. The counters and Role are filled in by
// listings (they describe the calling user's view of the room).
type Room struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	IsPrivate      bool      `json:"is_private"`
	IsDM           bool      `json:"is_dm"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Role           string    `json:"role,omitempty"`
	MemberCount    int       `json:"member_count,omitempty"`
	Unread         int       `json:"unread,omitempty"`
	UnreadMentions int       `json:"unread_mentions,omitempty"`
	Peer           string    `json:"peer,omitempty"`
}

// Member mirrors a room member listing entry.
type Member struct {
	UserID   string    `json:"user_id"`
	Username string    `json:"username"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// Invite mirrors an invite description (never the code itself).
type Invite struct {
	ID        string     `json:"id"`
	RoomID    string     `json:"room_id"`
	ExpiresAt time.Time  `json:"expires_at"`
	MaxUses   int        `json:"max_uses"`
	Uses      int        `json:"uses"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// createInviteResponse is the one-time reveal of a new invite code.
type createInviteResponse struct {
	Code   string `json:"code"`
	Invite Invite `json:"invite"`
}

// SessionInfo mirrors a signed-in session (refresh-token family).
type SessionInfo struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastActive time.Time `json:"last_active"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Message mirrors the backend's message JSON. Type is one of
// text, diff, code, log, commit.
type Message struct {
	ID        string     `json:"id"`
	RoomID    string     `json:"room_id"`
	UserID    string     `json:"user_id"`
	Username  string     `json:"username"`
	Content   string     `json:"content"`
	Type      string     `json:"type"`
	CreatedAt time.Time  `json:"created_at"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
	Deleted   bool       `json:"deleted,omitempty"`
	ReplyTo   string     `json:"reply_to,omitempty"`
	Mentions  []string   `json:"mentions,omitempty"`
	RoomName  string     `json:"room_name,omitempty"`
}

// statusResponse is the generic {"status": "..."} body used by join/leave.
type statusResponse struct {
	Status string `json:"status"`
}

// Realtime event types mirror the backend's event envelope
// (internal/event). Payload is raw JSON decoded per type by the chat view.
const (
	eventMessageNew     = "message.new"
	eventMessageEdited  = "message.edited"
	eventMessageDeleted = "message.deleted"
	eventTyping         = "typing"
	eventPresenceState  = "presence.state"
	eventPresenceJoin   = "presence.join"
	eventPresenceLeave  = "presence.leave"
)

// wsEvent mirrors the backend's realtime envelope: {type, room_id, payload}.
type wsEvent struct {
	Type    string          `json:"type"`
	RoomID  string          `json:"room_id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// presencePayload is the body of presence.state (online list) and
// presence.join/leave (a single username).
type presencePayload struct {
	Online   []string `json:"online,omitempty"`
	Username string   `json:"username,omitempty"`
}

// typingPayload is the body of a typing relay.
type typingPayload struct {
	Username string `json:"username"`
}
