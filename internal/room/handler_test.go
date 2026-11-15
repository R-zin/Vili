package room

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
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
	invites   map[string]*Invite // by code hash
	usernames map[string]uuid.UUID
	read      map[[2]uuid.UUID]bool
	createErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		rooms:     make(map[uuid.UUID]Room),
		members:   make(map[uuid.UUID]map[uuid.UUID]MemberRole),
		invites:   make(map[string]*Invite),
		usernames: make(map[string]uuid.UUID),
		read:      make(map[[2]uuid.UUID]bool),
	}
}

func (f *fakeRepo) Create(ctx context.Context, ownerID uuid.UUID, name, description string, private bool) (Room, error) {
	if f.createErr != nil {
		return Room{}, f.createErr
	}
	for _, r := range f.rooms {
		if r.Name == name {
			return Room{}, ErrNameTaken
		}
	}
	room := Room{ID: uuid.New(), Name: name, Description: description, IsPrivate: private, CreatedBy: ownerID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.rooms[room.ID] = room
	f.members[room.ID] = map[uuid.UUID]MemberRole{ownerID: RoleOwner}
	return room, nil
}

func (f *fakeRepo) List(ctx context.Context, userID uuid.UUID, flt ListFilter) ([]Summary, error) {
	out := make([]Summary, 0)
	for _, r := range f.rooms {
		role, member := f.members[r.ID][userID]
		if !member && (r.IsPrivate || flt.MineOnly) {
			continue
		}
		if flt.Query != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(flt.Query)) {
			continue
		}
		out = append(out, Summary{Room: r, Role: role, MemberCount: len(f.members[r.ID])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
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

func (f *fakeRepo) Role(ctx context.Context, roomID, userID uuid.UUID) (MemberRole, error) {
	role, ok := f.members[roomID][userID]
	if !ok {
		return "", ErrNotMember
	}
	return role, nil
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

func (f *fakeRepo) Members(ctx context.Context, roomID uuid.UUID) ([]Member, error) {
	out := make([]Member, 0)
	for uid, role := range f.members[roomID] {
		out = append(out, Member{UserID: uid, Role: role})
	}
	return out, nil
}

func (f *fakeRepo) SetRole(ctx context.Context, roomID, userID uuid.UUID, role MemberRole) error {
	if _, ok := f.members[roomID][userID]; !ok {
		return ErrNotMember
	}
	f.members[roomID][userID] = role
	return nil
}

func (f *fakeRepo) Update(ctx context.Context, id uuid.UUID, p UpdateParams) (Room, error) {
	r, ok := f.rooms[id]
	if !ok {
		return Room{}, ErrNotFound
	}
	if p.Name != nil {
		for _, o := range f.rooms {
			if o.ID != id && o.Name == *p.Name {
				return Room{}, ErrNameTaken
			}
		}
		r.Name = *p.Name
	}
	if p.Description != nil {
		r.Description = *p.Description
	}
	if p.IsPrivate != nil {
		r.IsPrivate = *p.IsPrivate
	}
	f.rooms[id] = r
	return r, nil
}

func (f *fakeRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if _, ok := f.rooms[id]; !ok {
		return ErrNotFound
	}
	delete(f.rooms, id)
	delete(f.members, id)
	return nil
}

func (f *fakeRepo) MarkRead(ctx context.Context, roomID, userID uuid.UUID) error {
	if _, ok := f.members[roomID][userID]; !ok {
		return ErrNotMember
	}
	f.read[[2]uuid.UUID{roomID, userID}] = true
	return nil
}

func (f *fakeRepo) CreateInvite(ctx context.Context, roomID, createdBy uuid.UUID, codeHash string, expiresAt time.Time, maxUses int) (Invite, error) {
	inv := &Invite{ID: uuid.New(), RoomID: roomID, CreatedBy: createdBy, ExpiresAt: expiresAt, MaxUses: maxUses, CreatedAt: time.Now()}
	f.invites[codeHash] = inv
	return *inv, nil
}

func (f *fakeRepo) ListInvites(ctx context.Context, roomID uuid.UUID) ([]Invite, error) {
	out := make([]Invite, 0)
	for _, inv := range f.invites {
		if inv.RoomID == roomID {
			out = append(out, *inv)
		}
	}
	return out, nil
}

func (f *fakeRepo) RevokeInvite(ctx context.Context, roomID, inviteID uuid.UUID) error {
	for _, inv := range f.invites {
		if inv.ID == inviteID && inv.RoomID == roomID {
			now := time.Now()
			inv.RevokedAt = &now
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeRepo) RedeemInvite(ctx context.Context, codeHash string, userID uuid.UUID) (Room, error) {
	inv, ok := f.invites[codeHash]
	if !ok || inv.RevokedAt != nil || time.Now().After(inv.ExpiresAt) {
		return Room{}, ErrInviteInvalid
	}
	if _, member := f.members[inv.RoomID][userID]; member {
		return Room{}, ErrAlreadyMember
	}
	if inv.MaxUses > 0 && inv.Uses >= inv.MaxUses {
		return Room{}, ErrInviteInvalid
	}
	f.members[inv.RoomID][userID] = RoleMember
	inv.Uses++
	return f.rooms[inv.RoomID], nil
}

func (f *fakeRepo) OpenDM(ctx context.Context, userID uuid.UUID, peer string) (Room, error) {
	pid, ok := f.usernames[peer]
	if !ok {
		return Room{}, ErrUserNotFound
	}
	if pid == userID {
		return Room{}, ErrSelfDM
	}
	room := Room{ID: uuid.New(), Name: "dm:" + peer, IsPrivate: true, IsDM: true, CreatedBy: userID}
	f.rooms[room.ID] = room
	f.members[room.ID] = map[uuid.UUID]MemberRole{userID: RoleMember, pid: RoleMember}
	return room, nil
}

// fakeDisc records disconnect calls.
type fakeDisc struct {
	disconnected [][2]uuid.UUID
	closed       []uuid.UUID
}

func (d *fakeDisc) Disconnect(roomID, userID uuid.UUID) {
	d.disconnected = append(d.disconnected, [2]uuid.UUID{roomID, userID})
}
func (d *fakeDisc) CloseRoom(roomID uuid.UUID) { d.closed = append(d.closed, roomID) }

var testTokenService = mustTokenService()

func mustTokenService() *auth.TokenService {
	tokens, err := auth.NewTokenService([]byte("room-handler-test-secret-at-least-32b!"), time.Hour)
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
	h := NewHandler(repo, nil)
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
	h := NewHandler(newFakeRepo(), nil)
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
	h := NewHandler(newFakeRepo(), nil)
	req := newReq(http.MethodPost, "/v1/createroom", `{"name":`, "")
	rec := authed(t, uuid.New(), h.create, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestCreate_DuplicateName(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo, nil)
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
	h := NewHandler(repo, nil)
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
	h := NewHandler(repo, nil)
	userID := uuid.New()
	if _, err := repo.Create(context.Background(), userID, "one", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(context.Background(), userID, "two", "", false); err != nil {
		t.Fatal(err)
	}

	rec := authed(t, userID, h.list, newReq(http.MethodGet, "/v1/listrooms", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var rooms []Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &rooms); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("expected 2 rooms, got %d", len(rooms))
	}
}

func TestJoinAndLeave(t *testing.T) {
	repo := newFakeRepo()
	h := NewHandler(repo, nil)
	owner := uuid.New()
	other := uuid.New()
	created, err := repo.Create(context.Background(), owner, "club", "", false)
	if err != nil {
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
	h := NewHandler(newFakeRepo(), nil)
	req := newReq(http.MethodPost, "/v1/rooms/not-a-uuid/join", "", "not-a-uuid")
	rec := authed(t, uuid.New(), h.join, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad uuid, got %d", rec.Code)
	}
}

// ---- moderation, invites, DMs -------------------------------------------------

// pv sets path values on a request.
func pv(req *http.Request, kv ...string) *http.Request {
	for i := 0; i+1 < len(kv); i += 2 {
		req.SetPathValue(kv[i], kv[i+1])
	}
	return req
}

type fixture struct {
	repo  *fakeRepo
	h     *Handler
	disc  *fakeDisc
	room  Room
	owner uuid.UUID
	admin uuid.UUID
	mem   uuid.UUID
	out   uuid.UUID // not a member
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := newFakeRepo()
	disc := &fakeDisc{}
	f := &fixture{repo: repo, h: NewHandler(repo, disc), disc: disc,
		owner: uuid.New(), admin: uuid.New(), mem: uuid.New(), out: uuid.New()}
	room, err := repo.Create(context.Background(), f.owner, "arena", "", false)
	if err != nil {
		t.Fatal(err)
	}
	f.room = room
	repo.members[room.ID][f.admin] = RoleAdmin
	repo.members[room.ID][f.mem] = RoleMember
	return f
}

func (f *fixture) id() string { return f.room.ID.String() }

func TestKick_Permissions(t *testing.T) {
	f := newFixture(t)
	other := uuid.New()
	f.repo.members[f.room.ID][other] = RoleMember

	cases := []struct {
		name   string
		actor  uuid.UUID
		target uuid.UUID
		want   int
	}{
		{"member cannot kick", f.mem, other, http.StatusForbidden},
		{"outsider sees 404", f.out, other, http.StatusNotFound},
		{"admin cannot kick owner", f.admin, f.owner, http.StatusForbidden},
		{"cannot kick self", f.owner, f.owner, http.StatusBadRequest},
		{"unknown target", f.owner, uuid.New(), http.StatusNotFound},
		{"admin kicks member", f.admin, other, http.StatusOK},
		{"owner kicks admin", f.owner, f.admin, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := pv(newReq(http.MethodDelete, "/", "", ""), "id", f.id(), "userID", tc.target.String())
			rec := authed(t, tc.actor, f.h.kick, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if len(f.disc.disconnected) != 2 {
		t.Fatalf("expected 2 disconnects, got %d", len(f.disc.disconnected))
	}
}

func TestSetRole(t *testing.T) {
	f := newFixture(t)
	do := func(actor, target uuid.UUID, body string) int {
		req := pv(newReq(http.MethodPut, "/", body, ""), "id", f.id(), "userID", target.String())
		return authed(t, actor, f.h.setRole, req).Code
	}
	if got := do(f.owner, f.mem, `{"role":"admin"}`); got != http.StatusOK {
		t.Fatalf("promote: %d", got)
	}
	if f.repo.members[f.room.ID][f.mem] != RoleAdmin {
		t.Fatal("role not changed")
	}
	if got := do(f.admin, f.mem, `{"role":"member"}`); got != http.StatusForbidden {
		t.Fatalf("admin must not change roles: %d", got)
	}
	if got := do(f.owner, f.mem, `{"role":"owner"}`); got != http.StatusBadRequest {
		t.Fatalf("granting owner must be rejected: %d", got)
	}
	if got := do(f.owner, f.owner, `{"role":"member"}`); got != http.StatusBadRequest {
		t.Fatalf("self change must be rejected: %d", got)
	}
	if got := do(f.owner, f.out, `{"role":"admin"}`); got != http.StatusNotFound {
		t.Fatalf("non-member target: %d", got)
	}
	if got := do(f.owner, f.mem, `{`); got != http.StatusBadRequest {
		t.Fatalf("bad json: %d", got)
	}
}

func TestUpdateRoom(t *testing.T) {
	f := newFixture(t)
	do := func(actor uuid.UUID, body string) int {
		req := pv(newReq(http.MethodPatch, "/", body, ""), "id", f.id())
		return authed(t, actor, f.h.update, req).Code
	}
	if got := do(f.admin, `{"description":"new"}`); got != http.StatusOK {
		t.Fatalf("admin describe: %d", got)
	}
	if got := do(f.admin, `{"is_private":true}`); got != http.StatusForbidden {
		t.Fatalf("admin visibility: %d", got)
	}
	if got := do(f.owner, `{"is_private":true}`); got != http.StatusOK || !f.repo.rooms[f.room.ID].IsPrivate {
		t.Fatalf("owner visibility: %d", got)
	}
	if got := do(f.mem, `{"name":"x"}`); got != http.StatusForbidden {
		t.Fatalf("member update: %d", got)
	}
	if got := do(f.owner, `{}`); got != http.StatusBadRequest {
		t.Fatalf("empty update: %d", got)
	}
	if got := do(f.owner, `{"name":"dm:sneaky"}`); got != http.StatusBadRequest {
		t.Fatalf("reserved prefix: %d", got)
	}
	other, _ := f.repo.Create(context.Background(), f.owner, "taken", "", false)
	_ = other
	if got := do(f.owner, `{"name":"taken"}`); got != http.StatusConflict {
		t.Fatalf("name taken: %d", got)
	}
}

func TestDeleteRoom(t *testing.T) {
	f := newFixture(t)
	req := pv(newReq(http.MethodDelete, "/", "", ""), "id", f.id())
	if rec := authed(t, f.admin, f.h.delete, req); rec.Code != http.StatusForbidden {
		t.Fatalf("admin delete: %d", rec.Code)
	}
	req = pv(newReq(http.MethodDelete, "/", "", ""), "id", f.id())
	if rec := authed(t, f.owner, f.h.delete, req); rec.Code != http.StatusOK {
		t.Fatalf("owner delete: %d", rec.Code)
	}
	if _, ok := f.repo.rooms[f.room.ID]; ok {
		t.Fatal("room not deleted")
	}
	if len(f.disc.closed) != 1 || f.disc.closed[0] != f.room.ID {
		t.Fatalf("sockets not closed: %v", f.disc.closed)
	}
}

func TestPrivateRoom_JoinHidden(t *testing.T) {
	f := newFixture(t)
	priv, _ := f.repo.Create(context.Background(), f.owner, "secret", "", true)
	req := pv(newReq(http.MethodPost, "/", "", ""), "id", priv.ID.String())
	if rec := authed(t, f.out, f.h.join, req); rec.Code != http.StatusNotFound {
		t.Fatalf("private join must 404, got %d", rec.Code)
	}
	// Not listed for outsiders, listed for members.
	rec := authed(t, f.out, f.h.list, newReq(http.MethodGet, "/", "", ""))
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("private room leaked in listing")
	}
	rec = authed(t, f.owner, f.h.list, newReq(http.MethodGet, "/", "", ""))
	if !strings.Contains(rec.Body.String(), "secret") {
		t.Fatal("member must see private room")
	}
}

func TestList_FiltersAndLimit(t *testing.T) {
	f := newFixture(t)
	f.repo.Create(context.Background(), f.owner, "golang", "", false)
	rec := authed(t, f.out, f.h.list, newReq(http.MethodGet, "/v1/listrooms?q=gol", "", ""))
	var rooms []Summary
	_ = json.Unmarshal(rec.Body.Bytes(), &rooms)
	if len(rooms) != 1 || rooms[0].Name != "golang" {
		t.Fatalf("query filter: %+v", rooms)
	}
	rec = authed(t, f.out, f.h.list, newReq(http.MethodGet, "/v1/listrooms?mine=1", "", ""))
	_ = json.Unmarshal(rec.Body.Bytes(), &rooms)
	if len(rooms) != 0 {
		t.Fatalf("mine filter: %+v", rooms)
	}
	for _, bad := range []string{"limit=abc", "limit=0"} {
		rec = authed(t, f.out, f.h.list, newReq(http.MethodGet, "/v1/listrooms?"+bad, "", ""))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
}

func TestInviteLifecycle(t *testing.T) {
	f := newFixture(t)
	priv, _ := f.repo.Create(context.Background(), f.owner, "vault", "", true)
	pid := priv.ID.String()

	// Members cannot create invites; outsiders get 404.
	f.repo.members[priv.ID][f.mem] = RoleMember
	if rec := authed(t, f.mem, f.h.createInvite, pv(newReq(http.MethodPost, "/", "", ""), "id", pid)); rec.Code != http.StatusForbidden {
		t.Fatalf("member invite: %d", rec.Code)
	}
	if rec := authed(t, f.out, f.h.createInvite, pv(newReq(http.MethodPost, "/", "", ""), "id", pid)); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider invite: %d", rec.Code)
	}
	// Bounds.
	for _, body := range []string{`{"max_uses":-1}`, `{"max_uses":100000}`, `{"ttl_hours":-2}`, `{"ttl_hours":100000}`} {
		if rec := authed(t, f.owner, f.h.createInvite, pv(newReq(http.MethodPost, "/", body, ""), "id", pid)); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, rec.Code)
		}
	}

	rec := authed(t, f.owner, f.h.createInvite, pv(newReq(http.MethodPost, "/", `{"max_uses":1}`, ""), "id", pid))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite: %d %s", rec.Code, rec.Body.String())
	}
	var created createInviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if len(created.Code) < 20 || strings.ContainsAny(created.Code, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Fatalf("unexpected code %q", created.Code)
	}

	redeem := func(who uuid.UUID, code string) int {
		body, _ := json.Marshal(map[string]string{"code": code})
		return authed(t, who, f.h.redeemInvite, newReq(http.MethodPost, "/", string(body), "")).Code
	}
	if got := redeem(f.out, "bogus"); got != http.StatusNotFound {
		t.Fatalf("bogus code: %d", got)
	}
	if got := redeem(f.out, strings.ToUpper(created.Code)); got != http.StatusOK { // case-insensitive
		t.Fatalf("redeem: %d", got)
	}
	if got := redeem(f.out, created.Code); got != http.StatusConflict {
		t.Fatalf("already member: %d", got)
	}
	if got := redeem(uuid.New(), created.Code); got != http.StatusNotFound {
		t.Fatalf("used-up invite must 404: %d", got)
	}
	if got := redeem(f.out, ""); got != http.StatusBadRequest {
		t.Fatalf("empty code: %d", got)
	}

	// List and revoke.
	rec = authed(t, f.owner, f.h.listInvites, pv(newReq(http.MethodGet, "/", "", ""), "id", pid))
	var invs []Invite
	_ = json.Unmarshal(rec.Body.Bytes(), &invs)
	if len(invs) != 1 || invs[0].Uses != 1 {
		t.Fatalf("invites: %+v", invs)
	}
	if strings.Contains(rec.Body.String(), created.Code) {
		t.Fatal("listing must never reveal codes")
	}
	rec = authed(t, f.owner, f.h.revokeInvite, pv(newReq(http.MethodDelete, "/", "", ""), "id", pid, "inviteID", invs[0].ID.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rec = authed(t, f.owner, f.h.revokeInvite, pv(newReq(http.MethodDelete, "/", "", ""), "id", pid, "inviteID", uuid.New().String()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d", rec.Code)
	}
}

func TestInviteExpiry(t *testing.T) {
	f := newFixture(t)
	base := time.Now()
	f.h.now = func() time.Time { return base.Add(-48 * time.Hour) } // already expired when "now" is real
	rec := authed(t, f.owner, f.h.createInvite, pv(newReq(http.MethodPost, "/", `{"ttl_hours":1}`, ""), "id", f.id()))
	var created createInviteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	body, _ := json.Marshal(map[string]string{"code": created.Code})
	if rec := authed(t, f.out, f.h.redeemInvite, newReq(http.MethodPost, "/", string(body), "")); rec.Code != http.StatusNotFound {
		t.Fatalf("expired invite redeemed: %d", rec.Code)
	}
}

func TestMembersAndMarkRead(t *testing.T) {
	f := newFixture(t)
	rec := authed(t, f.mem, f.h.members, pv(newReq(http.MethodGet, "/", "", ""), "id", f.id()))
	var ms []Member
	_ = json.Unmarshal(rec.Body.Bytes(), &ms)
	if rec.Code != http.StatusOK || len(ms) != 3 {
		t.Fatalf("members: %d %d", rec.Code, len(ms))
	}
	if rec := authed(t, f.out, f.h.members, pv(newReq(http.MethodGet, "/", "", ""), "id", f.id())); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider members: %d", rec.Code)
	}
	if rec := authed(t, f.mem, f.h.markRead, pv(newReq(http.MethodPost, "/", "", ""), "id", f.id())); rec.Code != http.StatusOK {
		t.Fatalf("mark read: %d", rec.Code)
	}
	if !f.repo.read[[2]uuid.UUID{f.room.ID, f.mem}] {
		t.Fatal("read cursor not advanced")
	}
	if rec := authed(t, f.out, f.h.markRead, pv(newReq(http.MethodPost, "/", "", ""), "id", f.id())); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider mark read: %d", rec.Code)
	}
}

func TestOpenDM(t *testing.T) {
	f := newFixture(t)
	f.repo.usernames["bob"] = f.mem
	do := func(body string) int {
		return authed(t, f.owner, f.h.openDM, newReq(http.MethodPost, "/", body, "")).Code
	}
	if got := do(`{"username":"bob"}`); got != http.StatusOK {
		t.Fatalf("dm: %d", got)
	}
	if got := do(`{"username":"nobody"}`); got != http.StatusNotFound {
		t.Fatalf("unknown: %d", got)
	}
	if got := do(`{"username":""}`); got != http.StatusBadRequest {
		t.Fatalf("empty: %d", got)
	}
	f.repo.usernames["me"] = f.owner
	if got := do(`{"username":"me"}`); got != http.StatusBadRequest {
		t.Fatalf("self: %d", got)
	}
}

func TestLeave_Disconnects(t *testing.T) {
	f := newFixture(t)
	req := pv(newReq(http.MethodPost, "/", "", ""), "id", f.id())
	if rec := authed(t, f.mem, f.h.leave, req); rec.Code != http.StatusOK {
		t.Fatalf("leave: %d", rec.Code)
	}
	if len(f.disc.disconnected) != 1 || f.disc.disconnected[0] != [2]uuid.UUID{f.room.ID, f.mem} {
		t.Fatalf("expected disconnect, got %v", f.disc.disconnected)
	}
}

func TestInviteCodeHashing(t *testing.T) {
	a, b := HashInviteCode(" ABC "), HashInviteCode("abc")
	if a != b || len(a) != 64 {
		t.Fatal("hash must be case/space-insensitive sha256 hex")
	}
	c1, _ := newInviteCode()
	c2, _ := newInviteCode()
	if c1 == c2 {
		t.Fatal("codes must be unique")
	}
}

func TestUnauthenticatedHandlers(t *testing.T) {
	f := newFixture(t)
	for name, fn := range map[string]http.HandlerFunc{"create": f.h.create, "list": f.h.list, "join": f.h.join, "kick": f.h.kick} {
		req := pv(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")), "id", f.id(), "userID", uuid.New().String())
		rec := httptest.NewRecorder()
		fn(rec, req) // no auth context
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", name, rec.Code)
		}
	}
	_ = errors.New
}
