package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/event"
	"github.com/R-zin/vili/internal/httputil"
	"github.com/R-zin/vili/internal/metrics"
)

// TestServeWS_ThroughMiddlewareChain performs the full realtime handshake with
// the Phase-5 router-wide middleware in front (recovery, logging/metrics,
// headers, body cap). It proves the websocket hijack still works through
// httputil's statusWriter via http.ResponseController/Unwrap, and that an
// upgraded connection receives events end to end.
func TestServeWS_ThroughMiddlewareChain(t *testing.T) {
	tokens, err := auth.NewTokenService([]byte("phase5-ws-secret"), time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	hub := NewHub()
	members := newFakeMembership()
	usernames := &fakeUsernames{names: make(map[uuid.UUID]string)}

	mux := http.NewServeMux()
	NewHandler(hub, members, usernames).RegisterRoutes(mux, tokens.RequireWS)

	reg := metrics.New()
	// Mirror the production chain's crucial pieces: recovery + observation wrap
	// every request, including the websocket upgrade.
	chained := httputil.Chain(mux,
		httputil.Recover,
		httputil.Observe(reg, false),
		httputil.SecurityHeaders,
		httputil.MaxBodyBytes(1<<20),
	)

	srv := httptest.NewServer(chained)
	t.Cleanup(srv.Close)

	userID := uuid.New()
	usernames.names[userID] = "bob"
	roomID := uuid.New()
	members.add(roomID, userID)
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/rooms/" + roomID.String() + "/ws"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("dial through middleware chain: %v (resp %+v)", err, resp)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("security headers missing on the upgrade response: %q", got)
	}
	if e := readEvent(t, conn); e.Type != event.PresenceState {
		t.Fatalf("first event = %q, want presence.state", e.Type)
	}
}

// TestServeWS_ConnectionCap429 verifies the pre-upgrade per-user cap: with a
// cap of 1 and a connection already open, the next handshake for the same user
// is refused with 429 before hijacking.
func TestServeWS_ConnectionCap429(t *testing.T) {
	srv, tokens, hub, members, usernames := wsTestServer(t)
	hub.SetMaxConnsPerUser(1)
	roomID := uuid.New()

	userID := uuid.New()
	usernames.names[userID] = "alice"
	members.add(roomID, userID)
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// First connection occupies the single slot.
	first, _, err := dialWS(t, srv, roomID, token)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close(websocket.StatusNormalClosure, "done")
	// Read the presence state so the connection is fully established and joined.
	if e := readEvent(t, first); e.Type != event.PresenceState {
		t.Fatalf("first event = %q, want presence.state", e.Type)
	}

	// Second concurrent connection for the same user is refused pre-upgrade.
	_, resp, err := dialWS(t, srv, roomID, token)
	if err == nil {
		t.Fatal("expected the over-cap handshake to be refused")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %+v (err %v)", resp, err)
	}
}
