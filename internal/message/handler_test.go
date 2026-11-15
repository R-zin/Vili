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
	roles     map[uuid.UUID]map[uuid.UUID]string
	usernames map[uuid.UUID]string // userID -> username (join-at-read)
	listErr   error
	createErr error

	gotLimit  int
	gotQuery  string
	gotBefore *time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		messages:  make(map[uuid.UUID][]Message),
		rooms:     make(map[uuid.UUID]bool),
		members:   make(map[uuid.UUID]map[uuid.UUID]bool),
		usernames: make(map[uuid.UUID]string),
		roles:     make(map[uuid.UUID]map[uuid.UUID]string),
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
	if msg.ReplyTo != nil {
		rid, _, ok := f.find(*msg.ReplyTo)
		if !ok || rid != msg.RoomID {
			return ErrReplyNotFound
		}
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

func (f *fakeRepo) MemberRole(ctx context.Context, roomID, userID uuid.UUID) (string, error) {
	if !f.members[roomID][userID] {
		return "", ErrNotMember
	}
	if r := f.roles[roomID][userID]; r != "" {
		return r, nil
	}
	return "member", nil
}

func (f *fakeRepo) find(id uuid.UUID) (roomID uuid.UUID, idx int, ok bool) {
	for rid, msgs := range f.messages {
		for i, m := range msgs {
			if m.ID == id {
				return rid, i, true
			}
		}
	}
	return uuid.Nil, 0, false
}

func (f *fakeRepo) ByID(ctx context.Context, id uuid.UUID) (Message, error) {
	rid, i, ok := f.find(id)
	if !ok {
		return Message{}, ErrMessageNotFound
	}
	return f.messages[rid][i], nil
}

func (f *fakeRepo) Edit(ctx context.Context, id uuid.UUID, content string, cands []string) (Message, error) {
	rid, i, ok := f.find(id)
	if !ok || f.messages[rid][i].Deleted {
		return Message{}, ErrMessageNotFound
	}
	now := time.Now()
	f.messages[rid][i].Content, f.messages[rid][i].Mentions, f.messages[rid][i].EditedAt = content, cands, &now
	return f.messages[rid][i], nil
}

func (f *fakeRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rid, i, ok := f.find(id)
	if !ok || f.messages[rid][i].Deleted {
		return ErrMessageNotFound
	}
	f.messages[rid][i].Content, f.messages[rid][i].Deleted = "", true
	return nil
}

func (f *fakeRepo) Search(ctx context.Context, userID uuid.UUID, q string, roomID *uuid.UUID, limit int) ([]Message, error) {
	f.gotQuery = q
	out := []Message{}
	for rid, msgs := range f.messages {
		if !f.members[rid][userID] || (roomID != nil && *roomID != rid) {
			continue
		}
		for _, m := range msgs {
			if !m.Deleted && strings.Contains(m.Content, q) {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func (f *fakeRepo) IsMember(ctx context.Context, roomID, userID uuid.UUID) (bool, error) {
	return f.members[roomID][userID], nil
}

var testTokenService = mustTokenService()

func mustTokenService() *auth.TokenService {
	tokens, err := auth.NewTokenService([]byte("message-handler-test-secret-at-least-32b!"), time.Hour)
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

// ---- edit / delete / reply / mentions / search -------------------------------

type msgFixture struct {
	repo   *fakeRepo
	pub    *fakePublisher
	h      *Handler
	room   uuid.UUID
	author uuid.UUID
	other  uuid.UUID
	admin  uuid.UUID
	out    uuid.UUID
}

func newMsgFixture() *msgFixture {
	f := &msgFixture{repo: newFakeRepo(), pub: &fakePublisher{}, room: uuid.New(),
		author: uuid.New(), other: uuid.New(), admin: uuid.New(), out: uuid.New()}
	f.h = NewHandler(f.repo, f.pub)
	f.repo.rooms[f.room] = true
	for _, u := range []uuid.UUID{f.author, f.other, f.admin} {
		f.repo.addMember(f.room, u)
	}
	f.repo.roles[f.room] = map[uuid.UUID]string{f.admin: "admin"}
	return f
}

func (f *msgFixture) post(t *testing.T, body string) Message {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.SetPathValue("id", f.room.String())
	rec := authed(t, f.author, f.h.create, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
	var m Message
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return m
}

func (f *msgFixture) call(t *testing.T, who uuid.UUID, fn http.HandlerFunc, method, body string, msg uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.SetPathValue("id", f.room.String())
	req.SetPathValue("msgID", msg.String())
	return authed(t, who, fn, req)
}

func TestExtractMentions(t *testing.T) {
	cases := map[string][]string{
		"hi @alice and @bob.":  {"alice", "bob"},
		"@alice @alice":        {"alice"},
		"mail me a@b.com":      nil,
		"email@host @real":     {"real"},
		"(@paren)":             {"paren"},
		"@a-b_c.d ok":          {"a-b_c.d"},
		"trailing @dash- here": {"dash"},
		"no mentions":          nil,
		"@@double":             nil,
		"@":                    nil,
		"\n@newline":           {"newline"},
		"@u1 @u2 @u3 @u4 @u5 @u6 @u7 @u8 @u9 @u10 @u11": {"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9", "u10"},
	}
	for in, want := range cases {
		got := extractMentions(in)
		if len(got) != len(want) {
			t.Errorf("%q: got %v want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: got %v want %v", in, got, want)
			}
		}
	}
}

func TestCreate_MentionsAndReply(t *testing.T) {
	f := newMsgFixture()
	first := f.post(t, `{"content":"hello @other"}`)
	if len(first.Mentions) != 1 || first.Mentions[0] != "other" {
		t.Fatalf("mentions = %v", first.Mentions)
	}
	reply := f.post(t, `{"content":"re","reply_to":"`+first.ID.String()+`"}`)
	if reply.ReplyTo == nil || *reply.ReplyTo != first.ID {
		t.Fatalf("reply_to lost: %+v", reply)
	}
	// Reply to a message that doesn't exist in this room -> 400.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"content":"x","reply_to":"`+uuid.New().String()+`"}`))
	req.SetPathValue("id", f.room.String())
	if rec := authed(t, f.author, f.h.create, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad reply target: %d", rec.Code)
	}
}

func TestEdit(t *testing.T) {
	f := newMsgFixture()
	m := f.post(t, `{"content":"orig"}`)
	f.pub.events = nil

	if rec := f.call(t, f.other, f.h.edit, http.MethodPatch, `{"content":"hax"}`, m.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("edit by non-author: %d", rec.Code)
	}
	if rec := f.call(t, f.out, f.h.edit, http.MethodPatch, `{"content":"hax"}`, m.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("edit by outsider: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.edit, http.MethodPatch, `{"content":""}`, m.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty edit: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.edit, http.MethodPatch, `{`, m.ID); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.edit, http.MethodPatch, `{"content":"x"}`, uuid.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown message: %d", rec.Code)
	}
	rec := f.call(t, f.author, f.h.edit, http.MethodPatch, `{"content":"fixed @other"}`, m.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	var got Message
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Content != "fixed @other" || got.EditedAt == nil || len(got.Mentions) != 1 {
		t.Fatalf("edited message: %+v", got)
	}
	if len(f.pub.events) != 1 || f.pub.events[0].Type != event.MessageEdited {
		t.Fatalf("expected one message.edited broadcast, got %+v", f.pub.events)
	}
}

func TestEdit_WrongRoomAndDeleted(t *testing.T) {
	f := newMsgFixture()
	m := f.post(t, `{"content":"orig"}`)
	// Same message id addressed through a different room it isn't in.
	otherRoom := uuid.New()
	f.repo.rooms[otherRoom] = true
	f.repo.addMember(otherRoom, f.author)
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"content":"x"}`))
	req.SetPathValue("id", otherRoom.String())
	req.SetPathValue("msgID", m.ID.String())
	if rec := authed(t, f.author, f.h.edit, req); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-room edit: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.delete, http.MethodDelete, "", m.ID); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.edit, http.MethodPatch, `{"content":"x"}`, m.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("edit deleted: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.delete, http.MethodDelete, "", m.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("double delete: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.edit, http.MethodPatch, `{"content":"x"}`, uuid.Nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nil id: %d", rec.Code)
	}
}

func TestDelete_Permissions(t *testing.T) {
	f := newMsgFixture()
	m1 := f.post(t, `{"content":"one"}`)
	m2 := f.post(t, `{"content":"two"}`)
	f.pub.events = nil

	if rec := f.call(t, f.other, f.h.delete, http.MethodDelete, "", m1.ID); rec.Code != http.StatusForbidden {
		t.Fatalf("member deleting others': %d", rec.Code)
	}
	if rec := f.call(t, f.out, f.h.delete, http.MethodDelete, "", m1.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider delete: %d", rec.Code)
	}
	if rec := f.call(t, f.admin, f.h.delete, http.MethodDelete, "", m1.ID); rec.Code != http.StatusOK {
		t.Fatalf("admin moderation delete: %d", rec.Code)
	}
	if rec := f.call(t, f.author, f.h.delete, http.MethodDelete, "", m2.ID); rec.Code != http.StatusOK {
		t.Fatalf("author delete: %d", rec.Code)
	}
	if len(f.pub.events) != 2 || f.pub.events[0].Type != event.MessageDeleted {
		t.Fatalf("expected two message.deleted events, got %+v", f.pub.events)
	}
	if !strings.Contains(string(f.pub.events[0].Payload), m1.ID.String()) {
		t.Fatalf("payload should carry id: %s", f.pub.events[0].Payload)
	}
	if got, _ := f.repo.ByID(context.Background(), m1.ID); got.Content != "" || !got.Deleted {
		t.Fatalf("content must be erased: %+v", got)
	}
}

func TestBadIDs(t *testing.T) {
	f := newMsgFixture()
	for _, fn := range []http.HandlerFunc{f.h.edit, f.h.delete} {
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		req.SetPathValue("id", "nope")
		req.SetPathValue("msgID", uuid.New().String())
		if rec := authed(t, f.author, fn, req); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad room id: %d", rec.Code)
		}
		req = httptest.NewRequest(http.MethodDelete, "/", nil)
		req.SetPathValue("id", f.room.String())
		req.SetPathValue("msgID", "nope")
		if rec := authed(t, f.author, fn, req); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad message id: %d", rec.Code)
		}
	}
}

func TestSearch(t *testing.T) {
	f := newMsgFixture()
	f.post(t, `{"content":"needle in haystack"}`)
	get := func(who uuid.UUID, qs string) *httptest.ResponseRecorder {
		return authed(t, who, f.h.search, httptest.NewRequest(http.MethodGet, "/v1/search?"+qs, nil))
	}
	rec := get(f.other, "q=needle")
	var res []Message
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if rec.Code != http.StatusOK || len(res) != 1 {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(f.out, "q=needle"); !strings.Contains(rec.Body.String(), "[]") {
		t.Fatalf("outsider must see no results: %s", rec.Body.String())
	}
	for _, bad := range []string{"", "q=", "q=" + strings.Repeat("x", 201), "q=a&room_id=zzz", "q=a&limit=x"} {
		if rec := get(f.other, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d", bad, rec.Code)
		}
	}
	if rec := get(f.other, "q=needle&room_id="+f.room.String()); rec.Code != http.StatusOK {
		t.Fatalf("room-scoped: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/search?q=a", nil)
	rec = httptest.NewRecorder()
	f.h.search(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search: %d", rec.Code)
	}
}
