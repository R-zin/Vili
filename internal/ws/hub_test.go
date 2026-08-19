package ws

import (
	"testing"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/event"
)

// newTestClient builds an unconnected Client (nil conn) registered with the
// hub, for exercising registry/broadcast behavior without a network.
func newTestClient(h *Hub, roomID uuid.UUID, username string) *Client {
	c := &Client{
		hub:      h,
		roomID:   roomID,
		userID:   uuid.New(),
		username: username,
		send:     make(chan event.Event, sendBuffer),
		done:     make(chan struct{}),
	}
	return c
}

func TestJoinBroadcastFanout(t *testing.T) {
	h := NewHub()
	roomID := uuid.New()
	a := newTestClient(h, roomID, "alice")
	b := newTestClient(h, roomID, "bob")
	h.Join(roomID, a)
	h.Join(roomID, b)

	h.Broadcast(roomID, event.NewTyping(roomID, "alice"))

	for _, c := range []*Client{a, b} {
		select {
		case e := <-c.send:
			if e.Type != event.Typing {
				t.Errorf("got type %q, want %q", e.Type, event.Typing)
			}
		default:
			t.Errorf("client %s did not receive broadcast", c.username)
		}
	}
}

func TestBroadcastOnlyToRoom(t *testing.T) {
	h := NewHub()
	roomA, roomB := uuid.New(), uuid.New()
	inA := newTestClient(h, roomA, "alice")
	inB := newTestClient(h, roomB, "bob")
	h.Join(roomA, inA)
	h.Join(roomB, inB)

	h.Broadcast(roomA, event.NewTyping(roomA, "alice"))

	select {
	case <-inB.send:
		t.Error("client in another room received the broadcast")
	default:
	}
}

func TestLeaveRemovesAndCleansEmptyRoom(t *testing.T) {
	h := NewHub()
	roomID := uuid.New()
	c := newTestClient(h, roomID, "alice")
	h.Join(roomID, c)
	if got := h.clientCount(roomID); got != 1 {
		t.Fatalf("clientCount = %d, want 1", got)
	}
	h.Leave(roomID, c)
	if got := h.clientCount(roomID); got != 0 {
		t.Fatalf("after leave clientCount = %d, want 0", got)
	}
	if _, ok := h.rooms[roomID]; ok {
		t.Error("empty room entry was not removed")
	}
}

func TestPresenceDedupesUsernames(t *testing.T) {
	h := NewHub()
	roomID := uuid.New()
	// Same user connected twice (two devices), plus another user.
	h.Join(roomID, newTestClient(h, roomID, "alice"))
	h.Join(roomID, newTestClient(h, roomID, "alice"))
	h.Join(roomID, newTestClient(h, roomID, "bob"))

	online := h.Presence(roomID)
	if len(online) != 2 {
		t.Fatalf("Presence = %v, want 2 deduped names", online)
	}
	seen := map[string]bool{}
	for _, n := range online {
		seen[n] = true
	}
	if !seen["alice"] || !seen["bob"] {
		t.Errorf("Presence = %v, want alice and bob", online)
	}
}

// TestSlowConsumerDropped fills a client's buffer and asserts broadcast does
// not block and the client is closed (dropped) rather than stalling the room.
func TestSlowConsumerDropped(t *testing.T) {
	h := NewHub()
	roomID := uuid.New()
	slow := newTestClient(h, roomID, "slow")
	h.Join(roomID, slow)

	// Fill the bounded buffer.
	for i := 0; i < sendBuffer; i++ {
		slow.send <- event.NewTyping(roomID, "x")
	}
	// The next sendNow must not block; it drops the client instead.
	done := make(chan struct{})
	go func() {
		h.Broadcast(roomID, event.NewTyping(roomID, "x"))
		close(done)
	}()
	select {
	case <-done:
	case <-wait():
		t.Fatal("Broadcast blocked on a slow consumer")
	}
	if !slow.isDone() {
		t.Error("slow consumer was not closed/dropped")
	}
}

func TestCloseIdempotentAndClosesClients(t *testing.T) {
	h := NewHub()
	roomID := uuid.New()
	c := newTestClient(h, roomID, "alice")
	h.Join(roomID, c)

	h.Close()
	h.Close() // must not panic or double-close

	if !c.isDone() {
		t.Error("client was not closed by Hub.Close")
	}
	if got := h.clientCount(roomID); got != 0 {
		t.Errorf("after Close clientCount = %d, want 0", got)
	}
}

func TestJoinAfterCloseRejected(t *testing.T) {
	h := NewHub()
	roomID := uuid.New()
	h.Close()
	h.Join(roomID, newTestClient(h, roomID, "alice"))
	if got := h.clientCount(roomID); got != 0 {
		t.Errorf("join after Close registered a client (count %d), want 0", got)
	}
}
