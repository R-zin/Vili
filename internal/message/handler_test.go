package message

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
)

// fakeRepo is an in-memory Repository implementation for tests.
type fakeRepo struct {
	messages map[uuid.UUID][]Message
	rooms    map[uuid.UUID]bool
	listErr  error

	gotLimit  int
	gotBefore *time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		messages: make(map[uuid.UUID][]Message),
		rooms:    make(map[uuid.UUID]bool),
	}
}

func (f *fakeRepo) Create(ctx context.Context, msg *Message) error {
	if !f.rooms[msg.RoomID] {
		return ErrRoomNotFound
	}
	if msg.ID == uuid.Nil {
		msg.ID = uuid.New()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	f.messages[msg.RoomID] = append(f.messages[msg.RoomID], *msg)
	return nil
}

func (f *fakeRepo) ListByRoom(ctx context.Context, roomID uuid.UUID, limit int, before *time.Time) ([]Message, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if !f.rooms[roomID] {
		return nil, ErrRoomNotFound
	}
	f.gotLimit = limit
	f.gotBefore = before
	return f.messages[roomID], nil
}

var testTokenService = mustTokenService()

func mustTokenService() *auth.TokenService {
	tokens, err := auth.NewTokenService([]byte("message-handler-test-secret"), time.Hour)
	if err != nil {
		panic(err)
	}
	return tokens
}

func authed(t *testing.T, userID uuid.UUID, h http.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	token, err := testTokenService.Issue(userID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	testTokenService.Middleware(h).ServeHTTP(rec, req)
	return rec
}

func TestList_OK(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	for _, content := range []string{"first", "second"} {
		if err := repo.Create(context.Background(), &Message{RoomID: roomID, UserID: uuid.New(), Username: "alice", Content: content, Type: TypeText}); err != nil {
			t.Fatal(err)
		}
	}

	h := NewHandler(repo)
	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/"+roomID.String()+"/messages", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.list, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var messages []Message
	if err := json.Unmarshal(rec.Body.Bytes(), &messages); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Username != "alice" {
		t.Fatalf("expected joined username, got %+v", messages[0])
	}
	if repo.gotLimit != defaultLimit {
		t.Fatalf("expected default limit %d, repo saw %d", defaultLimit, repo.gotLimit)
	}
}

func TestList_RoomNotFound(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", uuid.New().String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestList_BadUUID(t *testing.T) {
	h := NewHandler(newFakeRepo())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", "nope")
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestList_BadLimit(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	h := NewHandler(repo)
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-integer limit, got %d", rec.Code)
	}
}

func TestList_BadBeforeCursor(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	h := NewHandler(repo)
	req := httptest.NewRequest(http.MethodGet, "/?before=not-a-time", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad before cursor, got %d", rec.Code)
	}
}

func TestList_BeforeCursorPassed(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	h := NewHandler(repo)
	cursor := time.Now().UTC().Truncate(time.Second)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5&before="+cursor.Format(time.RFC3339), nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if repo.gotLimit != 5 {
		t.Fatalf("expected limit 5, repo saw %d", repo.gotLimit)
	}
	if repo.gotBefore == nil || !repo.gotBefore.Equal(cursor) {
		t.Fatalf("expected before cursor %s, repo saw %v", cursor, repo.gotBefore)
	}
}

func TestList_StoreError(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	repo.listErr = errors.New("query failed")
	h := NewHandler(repo)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// TestParseLimit is a table test over the clamp/default/validation rules.
func TestParseLimit(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    int
		wantErr bool
	}{
		{"empty uses default", "", defaultLimit, false},
		{"valid", "25", 25, false},
		{"min boundary", "1", 1, false},
		{"max boundary", "100", 100, false},
		{"below min clamps", "0", minLimit, false},
		{"negative clamps", "-5", minLimit, false},
		{"above max clamps", "99999", maxLimit, false},
		{"non-integer errors", "abc", 0, true},
		{"float errors", "1.5", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimit(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parseLimit(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
