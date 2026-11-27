package ws

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/bus"
	"github.com/R-zin/vili/internal/event"
)

// busTimeout bounds one publish to the cluster bus.
const busTimeout = 5 * time.Second

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

	// bus carries broadcasts and evictions to every instance (including this
	// one). presence, when set, is the cluster-wide "who is online" store;
	// otherwise presence is this process's connections only.
	bus      bus.Bus
	presence PresenceStore
}

// NewHub builds an empty Hub with no per-user connection cap, delivering
// through an in-process bus (single instance).
func NewHub() *Hub {
	h := &Hub{rooms: make(map[uuid.UUID]map[*Client]struct{}), userCounts: make(map[uuid.UUID]int)}
	h.UseBus(bus.NewLocal())
	return h
}

// UseBus routes the hub's broadcasts and evictions through b and subscribes to
// it, so events published by any instance reach this hub's connections. Call
// it during wiring, before serving.
func (h *Hub) UseBus(b bus.Bus) {
	h.bus = b
	b.Subscribe(h.onBus)
}

// UsePresence makes presence cluster-wide via p. Call it during wiring.
func (h *Hub) UsePresence(p PresenceStore) { h.presence = p }

// onBus applies a bus message to this instance's connections.
func (h *Hub) onBus(m bus.Message) {
	switch m.Kind {
	case bus.KindRoomEvent:
		if m.Event != nil {
			h.broadcastLocal(m.Room, *m.Event)
		}
	case bus.KindDisconnect:
		h.closeMatching(func(room uuid.UUID, c *Client) bool { return room == m.Room && c.userID == m.User },
			websocket.StatusPolicyViolation, "no longer a member of this room")
	case bus.KindCloseRoom:
		h.closeMatching(func(room uuid.UUID, _ *Client) bool { return room == m.Room },
			websocket.StatusGoingAway, "room deleted")
	case bus.KindSessionRevoked:
		h.closeMatching(func(_ uuid.UUID, c *Client) bool {
			return c.userID == m.User && (m.Session == uuid.Nil || c.session == m.Session)
		}, websocket.StatusPolicyViolation, "session ended")
	}
}

// publish sends m through the bus, logging (not returning) failures: the
// local delivery has already happened and durable state is in Postgres.
func (h *Hub) publish(m bus.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	if err := h.bus.Publish(ctx, m); err != nil {
		slog.Warn("realtime: cluster publish failed; other instances miss this update", "kind", m.Kind, "error", err)
	}
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

// Broadcast delivers e to every connection in roomID on every instance.
func (h *Hub) Broadcast(roomID uuid.UUID, e event.Event) {
	h.publish(bus.Message{Kind: bus.KindRoomEvent, Room: roomID, Event: &e})
}

// broadcastLocal enqueues e for every client of this instance in roomID. It is
// non-blocking: a client whose buffer is full is dropped via its sendNow
// slow-consumer path.
func (h *Hub) broadcastLocal(roomID uuid.UUID, e event.Event) {
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

// Disconnect closes every connection userID holds to roomID, on every
// instance, used when the user is removed from (or leaves) the room so they
// stop receiving its traffic at once.
func (h *Hub) Disconnect(roomID, userID uuid.UUID) {
	h.publish(bus.Message{Kind: bus.KindDisconnect, Room: roomID, User: userID})
}

// CloseRoom closes every connection to roomID on every instance (deleted room).
func (h *Hub) CloseRoom(roomID uuid.UUID) {
	h.publish(bus.Message{Kind: bus.KindCloseRoom, Room: roomID})
}

// RevokeSession closes userID's connections on every instance — all of them
// when session is uuid.Nil, else only those opened with that session — and
// tells every instance to drop cached session state for the user.
func (h *Hub) RevokeSession(userID, session uuid.UUID) {
	h.publish(bus.Message{Kind: bus.KindSessionRevoked, User: userID, Session: session})
}

// closeMatching closes this instance's clients selected by match.
func (h *Hub) closeMatching(match func(room uuid.UUID, c *Client) bool, status websocket.StatusCode, reason string) {
	h.mu.RLock()
	var victims []*Client
	for room, set := range h.rooms {
		for c := range set {
			if match(room, c) {
				victims = append(victims, c)
			}
		}
	}
	h.mu.RUnlock()
	for _, c := range victims {
		c.close(status, reason)
	}
}

// Stats returns the number of live connections and of rooms with at least one
// connection, for metrics.
func (h *Hub) Stats() (conns, rooms int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, set := range h.rooms {
		conns += len(set)
	}
	return conns, len(h.rooms)
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

// Online returns who is connected to roomID: cluster-wide when a presence
// store is configured (falling back to this instance if it fails), else this
// instance's connections.
func (h *Hub) Online(ctx context.Context, roomID uuid.UUID) []string {
	if h.presence != nil {
		online, err := h.presence.Online(ctx, roomID)
		if err == nil {
			return online
		}
		slog.Warn("realtime: presence lookup failed; showing this instance only", "error", err)
	}
	return h.Presence(roomID)
}

// trackPresence records a connection opening (delta +1) or closing (-1) in the
// cluster presence store, if any.
func (h *Hub) trackPresence(ctx context.Context, c *Client, delta int) {
	if h.presence == nil {
		return
	}
	var err error
	if delta > 0 {
		err = h.presence.Add(ctx, c.roomID, c.userID, c.username)
	} else {
		err = h.presence.Remove(ctx, c.roomID, c.userID)
	}
	if err != nil {
		slog.Warn("realtime: presence update failed", "error", err)
	}
}

// LocalPresence lists every (room, user) this instance holds connections for,
// with their counts, so a presence store can be rebuilt after it lost state.
func (h *Hub) LocalPresence() []PresenceEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	type key struct{ room, user uuid.UUID }
	counts := map[key]*PresenceEntry{}
	var out []PresenceEntry
	for room, set := range h.rooms {
		for c := range set {
			k := key{room, c.userID}
			if e, ok := counts[k]; ok {
				e.Conns++
				continue
			}
			counts[k] = &PresenceEntry{Room: room, User: c.userID, Username: c.username, Conns: 1}
		}
	}
	for _, e := range counts {
		out = append(out, *e)
	}
	return out
}

// Presence returns the deduplicated usernames connected to roomID on this
// instance.
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
