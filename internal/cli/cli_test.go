package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeAPI records requests and serves canned responses so command tests can
// assert paths, methods, the Authorization header, and payload handling without
// a real server.
type fakeAPI struct {
	t *testing.T

	// last captures the most recent request for assertions.
	lastMethod string
	lastPath   string
	lastAuth   string
	lastBody   map[string]string
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	capture := func(w http.ResponseWriter, r *http.Request) {
		f.lastMethod = r.Method
		f.lastPath = r.URL.Path
		f.lastAuth = r.Header.Get("Authorization")
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		}
	}

	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		capture(w, r)
		_ = json.NewEncoder(w).Encode(loginResponse{
			Token: "tok-123",
			User:  User{ID: "u1", Username: "dev"},
		})
	})
	mux.HandleFunc("POST /v1/rooms/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		capture(w, r)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Message{ID: "m1", Content: f.lastBody["content"]})
	})
	mux.HandleFunc("GET /v1/rooms/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		capture(w, r)
		_ = json.NewEncoder(w).Encode([]Message{})
	})
	mux.HandleFunc("POST /v1/createroom", func(w http.ResponseWriter, r *http.Request) {
		capture(w, r)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Room{ID: "r1", Name: f.lastBody["name"]})
	})
	return mux
}

func newFakeServer(t *testing.T) (*httptest.Server, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{t: t}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv, f
}

// isolateConfig points the CLI's session file at a temp dir for the test.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestLoginSavesSession(t *testing.T) {
	isolateConfig(t)
	srv, api := newFakeServer(t)

	var out bytes.Buffer
	err := Run(context.Background(), &out, strings.NewReader(""), []string{"--server", srv.URL, "login", "dev", "secret"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if api.lastPath != "/v1/auth/login" || api.lastMethod != http.MethodPost {
		t.Fatalf("expected POST /v1/auth/login, got %s %s", api.lastMethod, api.lastPath)
	}

	sess, err := LoadSession()
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if sess.Token != "tok-123" {
		t.Fatalf("expected saved token tok-123, got %q", sess.Token)
	}
	if sess.User.Username != "dev" {
		t.Fatalf("expected saved username dev, got %q", sess.User.Username)
	}
}

func TestSendUsesSessionToken(t *testing.T) {
	isolateConfig(t)
	srv, api := newFakeServer(t)

	if err := (&Session{BaseURL: srv.URL, Token: "sess-tok", User: User{Username: "dev"}}).Save(); err != nil {
		t.Fatalf("save session: %v", err)
	}

	var out bytes.Buffer
	err := Run(context.Background(), &out, strings.NewReader(""), []string{"--server", srv.URL, "send", "r1", "hello", "there"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if api.lastPath != "/v1/rooms/r1/messages" || api.lastMethod != http.MethodPost {
		t.Fatalf("expected POST /v1/rooms/r1/messages, got %s %s", api.lastMethod, api.lastPath)
	}
	if api.lastAuth != "Bearer sess-tok" {
		t.Fatalf("expected saved bearer token, got %q", api.lastAuth)
	}
	if api.lastBody["content"] != "hello there" {
		t.Fatalf("expected joined message body, got %q", api.lastBody["content"])
	}
}

func TestErrorEnvelopeSurfaced(t *testing.T) {
	isolateConfig(t)
	// Server that always returns the uniform error envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "unauthorized", "message": "invalid or expired token", "status": 401},
		})
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	err := Run(context.Background(), &out, strings.NewReader(""), []string{"--server", srv.URL, "rooms"})
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %T (%v)", err, err)
	}
	if apiErr.Status != http.StatusUnauthorized || apiErr.Message != "invalid or expired token" {
		t.Fatalf("unexpected decoded error: %+v", apiErr)
	}
}

func TestNoCommandShowsUsage(t *testing.T) {
	isolateConfig(t)
	var out bytes.Buffer
	err := Run(context.Background(), &out, strings.NewReader(""), nil)
	if err == nil {
		t.Fatal("expected an error for no command")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("expected usage output, got %q", out.String())
	}
}

func TestPollAfterFiltersToNewerThanCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rooms/r1/messages" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]Message{
			{ID: "old", CreatedAt: mustTime(t, "2026-08-18T10:00:00Z")},
			{ID: "new", CreatedAt: mustTime(t, "2026-08-18T10:05:00Z")},
		})
	}))
	t.Cleanup(srv.Close)

	cursor := mustTime(t, "2026-08-18T10:00:00Z")
	messages, err := pollAfter(context.Background(), NewClient(srv.URL, "tok"), "r1", &cursor)
	if err != nil {
		t.Fatalf("pollAfter: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != "new" {
		t.Fatalf("expected only the newer message, got %+v", messages)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	return parsed
}
