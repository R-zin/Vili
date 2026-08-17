package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMiddleware_PopulatesContext(t *testing.T) {
	s := newService(t, time.Hour)
	userID := uuid.New()
	token, err := s.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var gotID uuid.UUID
	var gotOK bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotOK = UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Middleware(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected handler to run, got status %d", rec.Code)
	}
	if !gotOK || gotID != userID {
		t.Fatalf("expected context user id %s, got %s (ok=%v)", userID, gotID, gotOK)
	}
}

func TestMiddleware_Unauthorized(t *testing.T) {
	s := newService(t, time.Hour)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not run when authentication fails")
	})

	cases := []struct {
		name   string
		header string
	}{
		{"missing header", ""},
		{"wrong scheme", "Basic abc"},
		{"bearer without token", "Bearer"},
		{"bearer empty token", "Bearer "},
		{"garbage token", "Bearer not-a-jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			s.Middleware(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", rec.Code)
			}
		})
	}
}

func TestUserIDFromContext_Absent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := UserIDFromContext(req.Context()); ok {
		t.Fatal("expected no user id in a bare request context")
	}
}
