package ws

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/bus"
	"github.com/R-zin/vili/internal/event"
)

func client(h *Hub, room, user, session uuid.UUID, name string) *Client {
	c := &Client{hub: h, roomID: room, userID: user, session: session, username: name,
		send: make(chan event.Event, 8), done: make(chan struct{})}
	h.Join(room, c)
	return c
}

func TestRevokeSessionScope(t *testing.T) {
	h := NewHub()
	r1, r2 := uuid.New(), uuid.New()
	alice, bob := uuid.New(), uuid.New()
	laptop, phone := uuid.New(), uuid.New()
	aLaptop1 := client(h, r1, alice, laptop, "alice")
	aLaptop2 := client(h, r2, alice, laptop, "alice")
	aPhone := client(h, r1, alice, phone, "alice")
	b := client(h, r1, bob, uuid.New(), "bob")

	h.RevokeSession(alice, laptop) // one device: every room it had open
	if !aLaptop1.isDone() || !aLaptop2.isDone() || aPhone.isDone() || b.isDone() {
		t.Fatal("revoking a session must close exactly that session's sockets")
	}
	h.RevokeSession(alice, uuid.Nil) // everywhere
	if !aPhone.isDone() || b.isDone() {
		t.Fatal("revoking all sessions must close every socket of that user only")
	}
}

// Two hubs sharing one bus behave like two instances: a broadcast or eviction
// on one reaches connections held by the other.
func TestHubsShareABus(t *testing.T) {
	shared := bus.NewLocal()
	h1, h2 := NewHub(), NewHub()
	h1.UseBus(shared)
	h2.UseBus(shared)
	room, user := uuid.New(), uuid.New()
	onH2 := client(h2, room, user, uuid.New(), "bob")

	h1.Broadcast(room, event.NewTyping(room, "alice"))
	select {
	case e := <-onH2.send:
		if e.Type != event.Typing {
			t.Fatalf("got %s", e.Type)
		}
	default:
		t.Fatal("broadcast on instance 1 must reach instance 2's socket")
	}
	h1.Disconnect(room, user)
	if !onH2.isDone() {
		t.Fatal("kick on instance 1 must close the socket on instance 2")
	}
}

type fakePresence struct {
	adds, removes int
	online        []string
	err           error
}

func (f *fakePresence) Add(ctx context.Context, room, user uuid.UUID, name string) error {
	f.adds++
	return f.err
}
func (f *fakePresence) Remove(ctx context.Context, room, user uuid.UUID) error {
	f.removes++
	return f.err
}
func (f *fakePresence) Online(ctx context.Context, room uuid.UUID) ([]string, error) {
	return f.online, f.err
}

func TestOnlineUsesStoreWithLocalFallback(t *testing.T) {
	h := NewHub()
	room := uuid.New()
	c := client(h, room, uuid.New(), uuid.Nil, "local-user")
	if got := h.Online(context.Background(), room); len(got) != 1 || got[0] != "local-user" {
		t.Fatalf("no store: %v", got)
	}
	p := &fakePresence{online: []string{"a", "b"}}
	h.UsePresence(p)
	if got := h.Online(context.Background(), room); len(got) != 2 {
		t.Fatalf("store: %v", got)
	}
	h.trackPresence(context.Background(), c, +1)
	h.trackPresence(context.Background(), c, -1)
	if p.adds != 1 || p.removes != 1 {
		t.Fatalf("tracking: %+v", p)
	}
	p.err = errors.New("db down")
	if got := h.Online(context.Background(), room); len(got) != 1 || got[0] != "local-user" {
		t.Fatalf("store failure must fall back to local presence: %v", got)
	}
	h.trackPresence(context.Background(), c, +1) // logged, not fatal
}

func TestLocalPresenceSnapshot(t *testing.T) {
	h := NewHub()
	r1, r2, u1, u2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	client(h, r1, u1, uuid.Nil, "a")
	client(h, r1, u1, uuid.Nil, "a")
	client(h, r1, u2, uuid.Nil, "b")
	client(h, r2, u1, uuid.Nil, "a")
	snap := h.LocalPresence()
	sort.Slice(snap, func(i, j int) bool { return snap[i].Conns > snap[j].Conns })
	if len(snap) != 3 || snap[0].Conns != 2 || snap[0].User != u1 || snap[0].Room != r1 {
		t.Fatalf("snapshot: %+v", snap)
	}
}
