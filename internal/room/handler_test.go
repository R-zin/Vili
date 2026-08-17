package room

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
)

// fakeRepo is an in-memory Repository implementation for tests.
type fakeRepo struct {
	rooms     map[uuid.UUID]Room
	members   map[uuid.UUID]map[uuid.UUID]MemberRole
	createErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		rooms:   make(map[uuid.UUID]Room),
		members: make(map[uuid.UUID]map[uuid.UUID]MemberRole),
	}
}

func (f *fakeRepo) Create(ctx context.Context, ownerID uuid.UUID, name, description string) (Room, error) {
	if f.createErr != nil {
		return Room{}, f.createErr
	}
	for _, r := range f.rooms {
		if r.Name == name {
			return Room{}, ErrNameTaken
		}
	}
	room := Room{ID: uuid.New(), Name: name, Description: description, CreatedBy: ownerID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.rooms[room.ID] = room
	return room, nil
}

func (f *fakeRepo) List(ctx context.Context) ([]Room, error) {
	out := make([]Room, 0, len(f.rooms))
	for _, r := range f.rooms {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeRepo) ByID(ctx context.Context, id uuid.UUID) (Room, error) {
	r, ok := f.rooms[id]
	if !ok {
		return Room{}, ErrNotFound
	}
	return r, nil
}

func (f *fakeRepo) AddMember(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error {
	if _, ok := f.rooms[roomID]; !ok {
		return ErrNotFound
	}
	if f.members[roomID] == nil {
		f.members[roomID] = make(map[uuid.UUID]MemberRole)
	}
	if _, ok := f.members[roomID][userID]; ok {
		return ErrAlreadyMember
	}
	f.members[roomID][userID] = role
	return nil
}

func (f *fakeRepo) RemoveMember(ctx context.Context, roomID, userID uuid.UUID) error {
	if _, ok := f.rooms[roomID]; !ok {
		return ErrNotFound
	}
	if _, ok := f.members[roomID][userID]; !ok {
		return ErrNotMember
	}
	delete(f.members[roomID], userID)
	return nil
}

var testTokenService = mustTokenService()

func mustTokenService() *auth.TokenService {
	tokens, err := auth.NewTokenService([]byte("room-handler-test-secret"), time.Hour)
	if err != nil {
		panic(err)
	}
	return tokens
}

// authed issues a real token for userID and routes req through the actual auth
// middleware before invoking h, so the handler sees a populated context.
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

// newReq builds a request with an optional {id} path value.
func newReq(method, target, body, id string) *http.Request {
	var rdr *strings.Reader
	if body == "" {
		rdr = strings.NewReader("")
	} else {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if id != "" {
		req.SetPathValue("id", id)
	}
	return req
}

func TestCreate_Created(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo)
	userID := uuid.New()

	req := newReq(http.MethodPost, "/v1/createroom", `{"name":"general","description":"room"}`, "")
	rec := authed(t, userID, h.create, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var room Room
	if err := json.Unmarshal(rec.Body.Bytes(), &room); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if room.Name != "general" || room.CreatedBy != userID {
		t.Fatalf("unexpected room: %+v", room)
	}
	if got := repo.members[room.ID][userID]; got != RoleOwner {
		t.Fatalf("expected creator to be owner, got %q", got)
	}
}

func TestCreate_Validation(t *testing.T) {
	h := NewHandler(newFakeRepo())
	userID := uuid.New()
	cases := []struct {
		name string
		body string
	}{
		{"empty name", `{"name":"","description":"x"}`},
		{"long name", `{"name":"` + strings.Repeat("n", 121) + `"}`},
		{"long description", `{"name":"ok","description":"` + strings.Repeat("d", 501) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newReq(http.MethodPost, "/v1/createroom", tc.body, "")
			rec := authed(t, userID, h.create, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreate_BadJSON(t *testing.T) {
	h := NewHandler(newFakeRepo())
	req := newReq(http.MethodPost, "/v1/createroom", `{"name":`, "")
	rec := authed(t, uuid.New(), h.create, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreate_DuplicateName(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo)
	userID := uuid.New()

	first := newReq(http.MethodPost, "/v1/createroom", `{"name":"dup"}`, "")
	if rec := authed(t, userID, h.create, first); rec.Code != http.StatusCreated {
		t.Fatalf("setup create failed: %d", rec.Code)
	}
	second := newReq(http.MethodPost, "/v1/createroom", `{"name":"dup"}`, "")
	rec := authed(t, userID, h.create, second)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreate_StoreError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("db exploded")
	h := NewHandler(repo)
	req := newReq(http.MethodPost, "/v1/createroom", `{"name":"x"}`, "")
	rec := authed(t, uuid.New(), h.create, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "db exploded") {
		t.Fatal("internal error must not leak")
	}
}

func TestList_OK(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo)
	userID := uuid.New()
	if _, err := repo.Create(context.Background(), userID, "one", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(context.Background(), userID, "two", ""); err != nil {
		t.Fatal(err)
	}

	rec := authed(t, userID, h.list, newReq(http.MethodGet, "/v1/listrooms", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var rooms []Room
	if err := json.Unmarshal(rec.Body.Bytes(), &rooms); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("expected 2 rooms, got %d", len(rooms))
	}
}

func TestJoinAndLeave(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo)
	owner := uuid.New()
	other := uuid.New()
	created, err := repo.Create(context.Background(), owner, "club", "")
	if err != nil {
		t.Fatal(err)
	}
	// The owner is a member; 'other' joins and leaves.
	if err := repo.AddMember(context.Background(), created.ID, owner, RoleOwner); err != nil {
		t.Fatal(err)
	}

	// Join an existing room.
	join := newReq(http.MethodPost, "/v1/rooms/"+created.ID.String()+"/join", "", created.ID.String())
	if rec := authed(t, other, h.join, join); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 join, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Joining again conflicts.
	join = newReq(http.MethodPost, "/", "", created.ID.String())
	if rec := authed(t, other, h.join, join); rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 rejoin, got %d", rec.Code)
	}

	// Unknown room -> 404.
	join = newReq(http.MethodPost, "/", "", uuid.New().String())
	if rec := authed(t, other, h.join, join); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 unknown room on join, got %d", rec.Code)
	}

	// Leave.
	leave := newReq(http.MethodPost, "/", "", created.ID.String())
	if rec := authed(t, other, h.leave, leave); rec.Code != http.StatusOK {
		t.Fatalf("expected 200 leave, got %d", rec.Code)
	}

	// Leaving again -> not a member -> 404.
	leave = newReq(http.MethodPost, "/", "", created.ID.String())
	if rec := authed(t, other, h.leave, leave); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 leaving non-membership, got %d", rec.Code)
	}
}

func TestJoin_BadUUID(t *testing.T) {
	h := NewHandler(newFakeRepo())
	req := newReq(http.MethodPost, "/v1/rooms/not-a-uuid/join", "", "not-a-uuid")
	rec := authed(t, uuid.New(), h.join, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad uuid, got %d", rec.Code)
	}
}
