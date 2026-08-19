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

// loginResponse mirrors the backend's login body {token, user}.
type loginResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

// Room mirrors the backend's room JSON.
type Room struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Message mirrors the backend's message JSON. Type is one of
// text, diff, code, log, commit.
type Message struct {
	ID        string    `json:"id"`
	RoomID    string    `json:"room_id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
}

// statusResponse is the generic {"status": "..."} body used by join/leave.
type statusResponse struct {
	Status string `json:"status"`
}

// Realtime event types mirror the backend's event envelope
// (internal/event). Payload is raw JSON decoded per type by the chat view.
const (
	eventMessageNew    = "message.new"
	eventTyping        = "typing"
	eventPresenceState = "presence.state"
	eventPresenceJoin  = "presence.join"
	eventPresenceLeave = "presence.leave"
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
