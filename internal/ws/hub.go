package ws

import (
	"sync"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/event"
)

// Hub is the in-memory registry of live websocket connections grouped by room.
// It fans events out to a room's clients and tracks who is online. It never
// writes to a connection directly — it only enqueues into each client's
// bounded send channel so broadcasting never blocks on a slow consumer.
//
// All registry access is guarded by one RWMutex; the Hub is safe for
// concurrent use and for -race.
type Hub struct {
	mu sync.RWMutex
	// rooms maps a room id to its set of connected clients.
	rooms map[uuid.UUID]map[*Client]struct{}
	// userCounts maps a user id to its total live connections across all rooms,
	// enforcing MaxConnsPerUser so one account can't pin unbounded sockets.
	userCounts map[uuid.UUID]int
	// maxPerUser caps concurrent connections per user; <=0 means unlimited.
	maxPerUser int
	// closed reports whether Close has been called; a closed hub rejects new
	// joins so no client outlives shutdown.
	closed bool
}

// NewHub builds an empty Hub with no per-user connection cap.
func NewHub() *Hub {
	return &Hub{rooms: make(map[uuid.UUID]map[*Client]struct{}), userCounts: make(map[uuid.UUID]int)}
}

// SetMaxConnsPerUser sets the concurrent-connection cap applied per user. A
// value <= 0 disables the cap. It is set once during wiring.
func (h *Hub) SetMaxConnsPerUser(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxPerUser = n
}

// Join adds c to its room and increments the user's connection count. It
// returns false when the hub is closed (so late connections cannot register
// after shutdown) or when the user is already at the per-user cap.
func (h *Hub) Join(roomID uuid.UUID, c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	if h.maxPerUser > 0 && h.userCounts[c.userID] >= h.maxPerUser {
		return false
	}
	set, ok := h.rooms[roomID]
	if !ok {
		set = make(map[*Client]struct{})
		h.rooms[roomID] = set
	}
	set[c] = struct{}{}
	h.userCounts[c.userID]++
	return true
}

// Leave removes c from its room, deleting the room's entry when it empties
// and decrementing the user's connection count.
func (h *Hub) Leave(roomID uuid.UUID, c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set, ok := h.rooms[roomID]
	if !ok {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.rooms, roomID)
	}
	if h.userCounts[c.userID] > 0 {
		h.userCounts[c.userID]--
		if h.userCounts[c.userID] == 0 {
			delete(h.userCounts, c.userID)
		}
	}
}

// Broadcast enqueues e for every client in roomID. It is non-blocking: a
// client whose buffer is full is dropped via its sendNow slow-consumer path.
func (h *Hub) Broadcast(roomID uuid.UUID, e event.Event) {
	h.mu.RLock()
	set := h.rooms[roomID]
	clients := make([]*Client, 0, len(set))
	for c := range set {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.sendNow(e)
	}
}

// ConnCount returns the number of live connections held by userID across all
// rooms. The handler uses it to reject an over-cap upgrade with 429 before
// hijacking; Join remains the authoritative atomic check.
func (h *Hub) ConnCount(userID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.userCounts[userID]
}

// MaxConnsPerUser returns the configured per-user cap (<=0 means unlimited).
func (h *Hub) MaxConnsPerUser() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.maxPerUser
}

// Presence returns the deduplicated usernames currently connected to roomID.
func (h *Hub) Presence(roomID uuid.UUID) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := h.rooms[roomID]
	seen := make(map[string]struct{}, len(set))
	online := make([]string, 0, len(set))
	for c := range set {
		if _, dup := seen[c.username]; dup {
			continue
		}
		seen[c.username] = struct{}{}
		online = append(online, c.username)
	}
	return online
}

// Close marks the hub closed and closes every connected client with
// StatusGoingAway. It is idempotent and is called during server shutdown so
// websockets don't outlive the HTTP drain.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := make([]*Client, 0)
	for _, set := range h.rooms {
		for c := range set {
			clients = append(clients, c)
		}
	}
	h.rooms = make(map[uuid.UUID]map[*Client]struct{})
	h.userCounts = make(map[uuid.UUID]int)
	h.mu.Unlock()

	for _, c := range clients {
		c.close(websocket.StatusGoingAway, "server shutting down")
	}
}

// clientCount reports the number of connected clients in a room; used in tests.
func (h *Hub) clientCount(roomID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[roomID])
}
