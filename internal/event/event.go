// Package event defines the realtime wire envelope shared by the websocket
// hub and the message feature's broadcast. It is a tiny dependency-free leaf
// so that feature packages (message, ws) can both reference the envelope
// without importing each other, preserving the rule that features never
// import one another.
package event

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// Event types carried over the websocket. message.new is broadcast to a room
// when a message is posted over REST; presence.state is sent to a single
// client right after it connects; presence.join/leave are broadcast as
// members connect and disconnect; typing is relayed ephemerally (never
// persisted) while a member composes.
const (
	MessageNew    = "message.new"
	Typing        = "typing"
	PresenceState = "presence.state"
	PresenceJoin  = "presence.join"
	PresenceLeave = "presence.leave"
)

// Event is the JSON envelope exchanged over the websocket. Payload is
// type-specific: a message.Message-JSON for message.new, a presence payload
// for presence.*, and a typing payload for typing. Payload is kept as raw
// JSON so the hub stays agnostic about what it relays.
type Event struct {
	Type    string          `json:"type"`
	RoomID  uuid.UUID       `json:"room_id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// presencePayload is the body for presence.state (online list) and
// presence.join/leave (a single username).
type presencePayload struct {
	Online   []string `json:"online,omitempty"`
	Username string   `json:"username,omitempty"`
}

// typingPayload is the body for a typing relay.
type typingPayload struct {
	Username string `json:"username"`
}

// ErrMalformed is returned by decoders when an event cannot be understood.
var ErrMalformed = errors.New("malformed event")

// NewMessage builds a message.new event whose payload is the encoded message.
// v must marshal to the backend message JSON (id, room_id, user_id, username,
// content, type, created_at).
func NewMessage(roomID uuid.UUID, v any) (Event, error) {
	p, err := json.Marshal(v)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: MessageNew, RoomID: roomID, Payload: p}, nil
}

// NewPresenceState builds a presence.state event carrying the online usernames.
func NewPresenceState(roomID uuid.UUID, online []string) Event {
	p, _ := json.Marshal(presencePayload{Online: online})
	return Event{Type: PresenceState, RoomID: roomID, Payload: p}
}

// NewPresenceJoin builds a presence.join event for a username.
func NewPresenceJoin(roomID uuid.UUID, username string) Event {
	p, _ := json.Marshal(presencePayload{Username: username})
	return Event{Type: PresenceJoin, RoomID: roomID, Payload: p}
}

// NewPresenceLeave builds a presence.leave event for a username.
func NewPresenceLeave(roomID uuid.UUID, username string) Event {
	p, _ := json.Marshal(presencePayload{Username: username})
	return Event{Type: PresenceLeave, RoomID: roomID, Payload: p}
}

// NewTyping builds a typing event for a username.
func NewTyping(roomID uuid.UUID, username string) Event {
	p, _ := json.Marshal(typingPayload{Username: username})
	return Event{Type: Typing, RoomID: roomID, Payload: p}
}

// TypingUsername extracts the username from a typing event's payload.
func TypingUsername(e Event) (string, error) {
	if e.Type != Typing {
		return "", ErrMalformed
	}
	var p typingPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return "", ErrMalformed
	}
	return p.Username, nil
}
