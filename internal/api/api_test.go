package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/message"
	"github.com/R-zin/vili/internal/room"
	"github.com/R-zin/vili/internal/user"
)

// fakePinger implements Pinger for readiness tests.
type fakePinger struct{ err error }

func (f fakePinger) PingContext(ctx context.Context) error { return f.err }

func newTestTokens(t *testing.T) *auth.TokenService {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte("api-test-secret"), time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return tokens
}

// newRouter builds the router against in-memory fakes (no database).
func newRouter(t *testing.T, p Pinger) http.Handler {
	t.Helper()
	tokens := newTestTokens(t)
	return NewRouter(
		user.NewHandler(nil, tokens), // repo unused by health/ready/protected-no-token
		room.NewHandler(nil),
		message.NewHandler(nil, nil),
		nil, // realtime handler unused by these router tests
		tokens,
		p,
	)
}

func TestHealth_Liveness(t *testing.T) {
	// Liveness must not touch the database; a nil Pinger proves it.
	h := newRouter(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); body == "" || body == "{}" {
		t.Fatalf("expected a liveness body, got %q", body)
	}
}

func TestReady_OK(t *testing.T) {
	h := newRouter(t, fakePinger{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestReady_Unavailable(t *testing.T) {
	h := newRouter(t, fakePinger{err: errors.New("connection refused")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	if body := rec.Body.String(); body == "" || strings.Contains(body, "connection refused") {
		t.Fatalf("readiness must not leak internal errors, got %q", body)
	}
}

// TestProtectedRoutesRequireAuth verifies every protected route returns 401
// when no Authorization header is present.
func TestProtectedRoutesRequireAuth(t *testing.T) {
	h := newRouter(t, fakePinger{})
	protected := []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/createroom"},
		{http.MethodGet, "/v1/listrooms"},
		{http.MethodPost, "/v1/rooms/00000000-0000-0000-0000-000000000000/join"},
		{http.MethodPost, "/v1/rooms/00000000-0000-0000-0000-000000000000/leave"},
		{http.MethodGet, "/v1/rooms/00000000-0000-0000-0000-000000000000/messages"},
	}
	for _, p := range protected {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(p.method, p.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 for %s %s, got %d", p.method, p.path, rec.Code)
			}
		})
	}
}
