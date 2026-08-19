package message

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/event"
)

// fakeRepo is an in-memory Repository implementation for tests.
type fakeRepo struct {
	messages  map[uuid.UUID][]Message
	rooms     map[uuid.UUID]bool
	members   map[uuid.UUID]map[uuid.UUID]bool // roomID -> userID -> member
	usernames map[uuid.UUID]string             // userID -> username (join-at-read)
	listErr   error
	createErr error

	gotLimit  int
	gotBefore *time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		messages:  make(map[uuid.UUID][]Message),
		rooms:     make(map[uuid.UUID]bool),
		members:   make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames: make(map[uuid.UUID]string),
	}
}

// fakePublisher captures broadcasts the handler makes after a post.
type fakePublisher struct {
	events []event.Event
}

func (p *fakePublisher) Broadcast(roomID uuid.UUID, e event.Event) {
	p.events = append(p.events, e)
}

// addMember registers userID as a member of roomID in the fake.
func (f *fakeRepo) addMember(roomID, userID uuid.UUID) {
	if f.members[roomID] == nil {
		f.members[roomID] = make(map[uuid.UUID]bool)
	}
	f.members[roomID][userID] = true
}

func (f *fakeRepo) Create(ctx context.Context, msg *Message) error {
	if f.createErr != nil {
		return f.createErr
	}
	if !f.rooms[msg.RoomID] {
		return ErrRoomNotFound
	}
	if msg.ID == uuid.Nil {
		msg.ID = uuid.New()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	if msg.Type == "" {
		msg.Type = TypeText
	}
	// Mirror the production JOIN: Create fills the author's username from the
	// usernames map when the caller has not set one.
	if msg.Username == "" {
		msg.Username = f.usernames[msg.UserID]
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

func (f *fakeRepo) IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error) {
	return f.members[roomID][userID], nil
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

	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/"+roomID.String()+"/messages", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.list, req)

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
	h := NewHandler(repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", uuid.New().String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestList_BadUUID(t *testing.T) {
	h := NewHandler(newFakeRepo(), nil)
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
	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/?limit=abc", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.list, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-integer limit, got %d", rec.Code)
	}
}

func TestList_BadBeforeCursor(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/?before=not-a-time", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.list, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad before cursor, got %d", rec.Code)
	}
}

func TestList_BeforeCursorPassed(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	cursor := time.Now().UTC().Truncate(time.Second)
	req := httptest.NewRequest(http.MethodGet, "/?limit=5&before="+cursor.Format(time.RFC3339), nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.list, req)
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
	userID := uuid.New()
	repo.addMember(roomID, userID)
	repo.listErr = errors.New("query failed")
	h := NewHandler(repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.list, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// TestList_NotMember asserts history is members-only: an authenticated user
// who is not a member of an existing room gets the same 404 as an unknown room.
func TestList_NotMember(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true // room exists, but the caller is not a member
	h := NewHandler(repo, nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.list, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-member, got %d", rec.Code)
	}
}

func TestCreate_OK(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)

	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":"hello world"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms/"+roomID.String()+"/messages", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var got Message
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Content != "hello world" {
		t.Fatalf("expected content persisted, got %+v", got)
	}
	if got.Type != TypeText {
		t.Fatalf("expected default type text, got %q", got.Type)
	}
	if got.UserID != userID {
		t.Fatalf("expected author %s, got %s", userID, got.UserID)
	}
}

func TestCreate_NotMember(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true // exists, caller not a member
	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, uuid.New(), h.create, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-member, got %d", rec.Code)
	}
}

func TestCreate_BadUUID(t *testing.T) {
	h := NewHandler(newFakeRepo(), nil)
	body := strings.NewReader(`{"content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", "nope")
	rec := authed(t, uuid.New(), h.create, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreate_MalformedJSON(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d", rec.Code)
	}
}

func TestCreate_EmptyContent(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":""}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty content, got %d", rec.Code)
	}
}

func TestCreate_BadType(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":"hi","type":"gif"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad type, got %d", rec.Code)
	}
}

func TestCreate_StoreError(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	repo.createErr = errors.New("insert failed")
	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

// TestParseLimit is a table test over the clamp/default/validation rules.
// TestCreate_Broadcasts asserts a successful post publishes a message.new
// event carrying the joined author username to the room's live connections.
func TestCreate_Broadcasts(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)
	repo.usernames[userID] = "alice"

	pub := &fakePublisher{}
	h := NewHandler(repo, pub)
	body := strings.NewReader(`{"content":"hello world"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(pub.events) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(pub.events))
	}
	e := pub.events[0]
	if e.Type != event.MessageNew {
		t.Fatalf("expected type %q, got %q", event.MessageNew, e.Type)
	}
	if e.RoomID != roomID {
		t.Fatalf("expected room %s, got %s", roomID, e.RoomID)
	}
	var got Message
	if err := json.Unmarshal(e.Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.Username != "alice" {
		t.Fatalf("expected broadcast to carry author username, got %+v", got)
	}
	if got.Content != "hello world" {
		t.Fatalf("expected broadcast payload content, got %+v", got)
	}
}

// TestCreate_NilPublisherNoPanic guards the REST-only path: a nil publisher
// must not break posting.
func TestCreate_NilPublisherNoPanic(t *testing.T) {
	repo := newFakeRepo()
	roomID := uuid.New()
	repo.rooms[roomID] = true
	userID := uuid.New()
	repo.addMember(roomID, userID)

	h := NewHandler(repo, nil)
	body := strings.NewReader(`{"content":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.SetPathValue("id", roomID.String())
	rec := authed(t, userID, h.create, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 with nil publisher, got %d", rec.Code)
	}
}

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
