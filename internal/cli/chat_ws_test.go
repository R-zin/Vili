package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// syncBuffer is a goroutine-safe output sink for tests that drive the chat
// loop on one goroutine while asserting on its output from another.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWSURLDerivesSchemeAndPath(t *testing.T) {
	cases := []struct {
		base    string
		want    string
		wantErr bool
	}{
		{"http://localhost:8080", "ws://localhost:8080/v1/rooms/r1/ws", false},
		{"https://chat.example.com", "wss://chat.example.com/v1/rooms/r1/ws", false},
		{"http://localhost:8080/", "ws://localhost:8080/v1/rooms/r1/ws", false},
		{"ftp://x", "", true},
	}
	for _, tc := range cases {
		c := NewClient(tc.base, "tok")
		got, err := c.wsURL("r1")
		if tc.wantErr {
			if err == nil {
				t.Errorf("wsURL(%q) expected error, got %q", tc.base, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("wsURL(%q): %v", tc.base, err)
			continue
		}
		if got != tc.want {
			t.Errorf("wsURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// holdWSServer accepts one websocket connection at /v1/rooms/{id}/ws and, once
// open, pushes the supplied events to the client. It simulates the realtime
// backend without a database.
func holdWSServer(t *testing.T, push []wsEvent) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/rooms/{id}/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "bye")
		for _, e := range push {
			data, _ := json.Marshal(e)
			if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
				return
			}
		}
		// Keep the socket open until the client disconnects.
		_, _, _ = conn.Read(context.Background())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunRealtimeChatRendersLiveMessage is the core Phase-4 behavior: a message
// arriving over the socket renders without the user pressing Enter.
func TestRunRealtimeChatRendersLiveMessage(t *testing.T) {
	payload, _ := json.Marshal(Message{ID: "m1", Username: "alice", Content: "hello there", CreatedAt: time.Now()})
	srv := holdWSServer(t, []wsEvent{{Type: eventMessageNew, RoomID: "r1", Payload: payload}})

	cli := NewClient(srv.URL, "tok")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := cli.watch(ctx, "r1")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	push := make(chan wsEvent)
	readEvents(conn, push)
	// Input stays open (no EOF, no command) for the duration.
	lines := make(chan string)
	defer close(lines)

	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- runRealtimeChat(ctx, out, cli, "r1", conn, lines, push) }()

	// The message should render promptly, with no input from the user.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "alice: hello there") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "alice: hello there") {
		t.Fatalf("live message did not render; output was %q", out.String())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRealtimeChat did not return after cancel")
	}
}

func TestRenderEventVariants(t *testing.T) {
	msgPayload, _ := json.Marshal(Message{Username: "alice", Content: "hi", CreatedAt: time.Now()})
	joinPayload, _ := json.Marshal(presencePayload{Username: "bob"})
	statePayload, _ := json.Marshal(presencePayload{Online: []string{"alice", "bob"}})
	typingPayloadB, _ := json.Marshal(typingPayload{Username: "carol"})

	cases := []struct {
		name string
		e    wsEvent
		want string
	}{
		{"message", wsEvent{Type: eventMessageNew, RoomID: "r", Payload: msgPayload}, "alice: hi"},
		{"join", wsEvent{Type: eventPresenceJoin, RoomID: "r", Payload: joinPayload}, "bob joined"},
		{"leave", wsEvent{Type: eventPresenceLeave, RoomID: "r", Payload: joinPayload}, "bob left"},
		{"state", wsEvent{Type: eventPresenceState, RoomID: "r", Payload: statePayload}, "online: alice, bob"},
		{"typing", wsEvent{Type: eventTyping, RoomID: "r", Payload: typingPayloadB}, "carol is typing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			renderEvent(&out, tc.e)
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("renderEvent(%s) = %q, want substring %q", tc.name, out.String(), tc.want)
			}
		})
	}
}

// TestCmdChatFallsBackToPolling points chat at a server with no websocket route
// and asserts it degrades to the polling view rather than erroring out.
func TestCmdChatFallsBackToPolling(t *testing.T) {
	isolateConfig(t)
	srv, _ := newFakeServer(t) // serves history but no /ws, so realtime dial 404s

	var out bytes.Buffer
	err := Run(context.Background(), &out, strings.NewReader("/quit\n"),
		[]string{"--server", srv.URL, "--token", "tok", "chat", "r1"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "polling") {
		t.Errorf("expected fallback to polling, output: %q", got)
	}
}

// TestCmdChatConnectsRealtime stands up a real websocket endpoint and asserts
// chat enters the live view when the socket can be established.
func TestCmdChatConnectsRealtime(t *testing.T) {
	isolateConfig(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/rooms/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]Message{})
	})
	mux.HandleFunc("GET /v1/rooms/{id}/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "bye")
		// Hold the socket open until the client disconnects.
		_, _, _ = conn.Read(context.Background())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	err := Run(context.Background(), &out, strings.NewReader("/quit\n"),
		[]string{"--server", srv.URL, "--token", "tok", "chat", "r1"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "live chat") {
		t.Errorf("expected realtime live view, output: %q", got)
	}
}
