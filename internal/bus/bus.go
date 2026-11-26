// Package bus fans cluster-wide signals out to every server instance: room
// events (messages, presence, typing), socket evictions (kick, leave, room
// deletion) and session revocations. Every Publish is delivered to the local
// subscribers immediately and, with the Postgres bus, to every other instance
// through LISTEN/NOTIFY. With the Local bus a single process behaves exactly
// as before.
//
// Delivery is at-most-once: an instance that is disconnected from Postgres
// misses what is published meanwhile. Durable state (messages, memberships,
// sessions) always lives in Postgres; the bus only carries notifications, so
// a missed one costs a live update, never data or a security check (session
// caches also expire on their own; see auth.SessionCache).
package bus

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/event"
)

// Kinds of message.
const (
	// KindRoomEvent carries Event to every live connection in Room.
	KindRoomEvent = "room_event"
	// KindDisconnect closes User's connections to Room.
	KindDisconnect = "disconnect"
	// KindCloseRoom closes every connection to Room (it was deleted).
	KindCloseRoom = "close_room"
	// KindSessionRevoked invalidates cached session state for User and closes
	// their sockets: all of them when Session is uuid.Nil, else only those
	// opened with that session.
	KindSessionRevoked = "session_revoked"
)

// Message is one bus notification.
type Message struct {
	Kind    string       `json:"k"`
	Room    uuid.UUID    `json:"r,omitempty"`
	User    uuid.UUID    `json:"u,omitempty"`
	Session uuid.UUID    `json:"s,omitempty"`
	Event   *event.Event `json:"e,omitempty"`
}

// Handler receives messages. Handlers run on the publisher's goroutine for
// local delivery and on the listener goroutine for remote delivery, so they
// must not block.
type Handler func(Message)

// Bus publishes messages to every instance's subscribers.
type Bus interface {
	Publish(ctx context.Context, m Message) error
	Subscribe(h Handler)
}

// handlers is the shared subscriber list.
type handlers struct {
	mu sync.RWMutex
	hs []Handler
}

func (s *handlers) Subscribe(h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hs = append(s.hs, h)
}

func (s *handlers) deliver(m Message) {
	s.mu.RLock()
	hs := append([]Handler(nil), s.hs...)
	s.mu.RUnlock()
	for _, h := range hs {
		h(m)
	}
}

// Local is an in-process bus: Publish delivers to this process's subscribers
// only. It is the single-instance (and test) implementation.
type Local struct{ handlers }

// NewLocal builds a Local bus.
func NewLocal() *Local { return &Local{} }

// Publish delivers m to the local subscribers.
func (l *Local) Publish(_ context.Context, m Message) error {
	l.deliver(m)
	return nil
}
