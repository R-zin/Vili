package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/event"
)

// wait returns a timeout channel for "must not block" assertions.
func wait() <-chan time.Time { return time.After(2 * time.Second) }

// fakeMembership is an in-memory MembershipChecker.
type fakeMembership struct {
	members map[uuid.UUID]map[uuid.UUID]bool // roomID -> userID -> member
}

func newFakeMembership() *fakeMembership {
	return &fakeMembership{members: make(map[uuid.UUID]map[uuid.UUID]bool)}
}

func (f *fakeMembership) add(roomID, userID uuid.UUID) {
	if f.members[roomID] == nil {
		f.members[roomID] = make(map[uuid.UUID]bool)
	}
	f.members[roomID][userID] = true
}

func (f *fakeMembership) IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error) {
	return f.members[roomID][userID], nil
}

// fakeUsernames is an in-memory UsernameResolver.
type fakeUsernames struct {
	names map[uuid.UUID]string
	err   error
}

func (f *fakeUsernames) UsernameByID(ctx context.Context, id uuid.UUID) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	n, ok := f.names[id]
	if !ok {
		return "", errors.New("user not found")
	}
	return n, nil
}

// wsTestServer wires a hub + handler onto a mux behind real JWT auth and serves
// it over httptest (no database). It returns the server, the token service, and
// the fakes so tests can arrange membership and users.
func wsTestServer(t *testing.T) (*httptest.Server, *auth.TokenService, *Hub, *fakeMembership, *fakeUsernames) {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte("ws-handler-test-secret"), time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	hub := NewHub()
	members := newFakeMembership()
	usernames := &fakeUsernames{names: make(map[uuid.UUID]string)}

	mux := http.NewServeMux()
	NewHandler(hub, members, usernames).RegisterRoutes(mux, tokens.RequireWS)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tokens, hub, members, usernames
}

// dialWS performs the websocket handshake against the test server. token may be
// empty (unauthenticated).
func dialWS(t *testing.T, srv *httptest.Server, roomID uuid.UUID, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/rooms/" + roomID.String() + "/ws"
	opts := &websocket.DialOptions{}
	if token != "" {
		opts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + token}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, url, opts)
}

func readEvent(t *testing.T, c *websocket.Conn) event.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var e event.Event
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return e
}

func TestServeWS_NoToken401(t *testing.T) {
	srv, _, _, _, _ := wsTestServer(t)
	_, resp, err := dialWS(t, srv, uuid.New(), "")
	if err == nil {
		t.Fatal("expected handshake to fail without a token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %+v (err %v)", resp, err)
	}
}

func TestServeWS_BadUUID400(t *testing.T) {
	srv, tokens, _, _, _ := wsTestServer(t)
	userID := uuid.New()
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/rooms/not-a-uuid/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err == nil {
		t.Fatal("expected handshake to fail for a bad uuid")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %+v (err %v)", resp, err)
	}
}

func TestServeWS_NonMember404(t *testing.T) {
	srv, tokens, _, _, usernames := wsTestServer(t)
	userID := uuid.New()
	usernames.names[userID] = "mallory"
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Authenticated but not a member of this (existing) room.
	_, resp, err := dialWS(t, srv, uuid.New(), token)
	if err == nil {
		t.Fatal("expected handshake to fail for a non-member")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %+v (err %v)", resp, err)
	}
}

// TestServeWS_PresenceStateOnConnect asserts a member is upgraded and receives a
// presence.state frame listing who is already online.
func TestServeWS_PresenceStateOnConnect(t *testing.T) {
	srv, tokens, hub, members, usernames := wsTestServer(t)
	roomID := uuid.New()

	// Pre-seed one online member directly in the hub.
	existing := newTestClient(hub, roomID, "alice")
	hub.Join(roomID, existing)

	userID := uuid.New()
	usernames.names[userID] = "bob"
	members.add(roomID, userID)
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	conn, resp, err := dialWS(t, srv, roomID, token)
	if err != nil {
		t.Fatalf("dial: %v (resp %+v)", err, resp)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	e := readEvent(t, conn)
	if e.Type != event.PresenceState {
		t.Fatalf("first event type = %q, want %q", e.Type, event.PresenceState)
	}
	var p struct {
		Online []string `json:"online"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal presence payload: %v", err)
	}
	// The connecting client (bob) and the pre-seeded member (alice) are online.
	seen := map[string]bool{}
	for _, n := range p.Online {
		seen[n] = true
	}
	if len(p.Online) != 2 || !seen["alice"] || !seen["bob"] {
		t.Errorf("presence.state online = %v, want {alice, bob}", p.Online)
	}
}

// TestServeWS_MessageFanout connects two members via real websockets and asserts
// a hub broadcast reaches the other connection over the wire.
func TestServeWS_MessageFanout(t *testing.T) {
	srv, tokens, hub, members, usernames := wsTestServer(t)
	roomID := uuid.New()

	connect := func(name string) *websocket.Conn {
		userID := uuid.New()
		usernames.names[userID] = name
		members.add(roomID, userID)
		token, err := tokens.Issue(userID)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		conn, _, err := dialWS(t, srv, roomID, token)
		if err != nil {
			t.Fatalf("dial %s: %v", name, err)
		}
		return conn
	}

	alice := connect("alice")
	defer alice.Close(websocket.StatusNormalClosure, "done")
	bob := connect("bob")
	defer bob.Close(websocket.StatusNormalClosure, "done")

	// Each receives presence.state then presence.join as peers connect; drain
	// until we see the fan-out message. Broadcast from the hub as if a REST post.
	msgPayload := map[string]any{"content": "hi there", "username": "alice", "type": "text"}
	ev, err := event.NewMessage(roomID, msgPayload)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	hub.Broadcast(roomID, ev)

	got := readUntilType(t, alice, event.MessageNew)
	var m struct {
		Content  string `json:"content"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(got.Payload, &m); err != nil {
		t.Fatalf("unmarshal message payload: %v", err)
	}
	if m.Content != "hi there" || m.Username != "alice" {
		t.Errorf("fan-out message = %+v, want content/username set", m)
	}
}

// readUntilType reads events until one of the given type arrives (skipping
// presence/typing noise), failing on timeout.
func readUntilType(t *testing.T, c *websocket.Conn, typ string) event.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e := readEvent(t, c)
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("no event of type %q arrived before timeout", typ)
	return event.Event{}
}
