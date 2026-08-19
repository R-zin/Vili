package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func requireWS(t *testing.T, next http.HandlerFunc) http.Handler {
	t.Helper()
	tokens, err := NewTokenService([]byte("requirews-test-secret"), time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return tokens.RequireWS(next)
}

func TestRequireWS_HeaderStillWorks(t *testing.T) {
	tokens, _ := NewTokenService([]byte("requirews-test-secret"), time.Hour)
	userID := uuid.New()
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	var gotID interface{}
	h := requireWS(t, func(w http.ResponseWriter, r *http.Request) {
		id, _ := UserIDFromContext(r.Context())
		gotID = id
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotID != userID {
		t.Fatalf("context user id = %v, want %v", gotID, userID)
	}
}

func TestRequireWS_QueryTokenAccepted(t *testing.T) {
	tokens, _ := NewTokenService([]byte("requirews-test-secret"), time.Hour)
	userID := uuid.New()
	token, err := tokens.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	h := requireWS(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/?token="+token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for query token, got %d", rec.Code)
	}
}

func TestRequireWS_HeaderPrecedenceOverQuery(t *testing.T) {
	tokens, _ := NewTokenService([]byte("requirews-test-secret"), time.Hour)
	headerID, queryID := uuid.New(), uuid.New()
	headerTok, _ := tokens.Issue(headerID)
	queryTok, _ := tokens.Issue(queryID)

	var gotID interface{}
	h := requireWS(t, func(w http.ResponseWriter, r *http.Request) {
		id, _ := UserIDFromContext(r.Context())
		gotID = id
	})
	req := httptest.NewRequest(http.MethodGet, "/?token="+queryTok, nil)
	req.Header.Set("Authorization", "Bearer "+headerTok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if gotID != headerID {
		t.Fatalf("expected header token to win, got id %v", gotID)
	}
}

func TestRequireWS_NoCredentials401(t *testing.T) {
	h := requireWS(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no credentials, got %d", rec.Code)
	}
}
