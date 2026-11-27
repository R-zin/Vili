// Package ws is the realtime websocket feature: it owns the in-memory Hub
// (per-room connection registry), the per-connection Client pumps, and the
// membership-gated GET /v1/rooms/{id}/ws handler. Each process keeps its own
// connections in memory; broadcasts, evictions and session revocations travel
// between instances over the cluster bus (internal/bus), and presence can be
// made cluster-wide with a PresenceStore (PGPresence), so any number of
// instances can serve the same rooms.
//
// Like every feature package it is self-contained: it never imports the other
// feature packages. It depends on narrow interfaces (membership checking and
// username resolution) that the wiring layer satisfies with the message and
// user repositories, and it shares the realtime envelope with the message
// feature via the tiny github.com/R-zin/vili/internal/event leaf so no feature
// imports another.
package ws

import (
	"context"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/event"
)

// Publisher is how the message feature broadcasts a freshly-posted message to
// a room's live connections without importing this package. The Hub satisfies
// it; tests use an in-memory fake.
type Publisher interface {
	Broadcast(roomID uuid.UUID, e event.Event)
}

// MembershipChecker reports whether a user belongs to a room. It is satisfied
// by the message repository's IsMember; the ws package uses it to gate the
// websocket upgrade exactly as REST history/posts are gated.
type MembershipChecker interface {
	IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error)
}

// UsernameResolver resolves a user id to its display name. Websocket auth
// yields only a user id (from the JWT subject), so presence and typing need a
// lookup; it is satisfied by the user repository's UsernameByID.
type UsernameResolver interface {
	UsernameByID(ctx context.Context, id uuid.UUID) (string, error)
}

// PresenceEntry is one (room, user) with its connection count on an instance.
type PresenceEntry struct {
	Room     uuid.UUID
	User     uuid.UUID
	Username string
	Conns    int
}

// PresenceStore tracks who is online across all instances. Add/Remove are
// called as this instance's sockets open and close; Online answers for the
// whole cluster.
type PresenceStore interface {
	Add(ctx context.Context, roomID, userID uuid.UUID, username string) error
	Remove(ctx context.Context, roomID, userID uuid.UUID) error
	Online(ctx context.Context, roomID uuid.UUID) ([]string, error)
}
