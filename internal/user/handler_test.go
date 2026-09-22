package user

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
	byUsername map[string]User

	createErr error
	lookupErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byUsername: make(map[string]User)}
}

func (f *fakeRepo) Create(ctx context.Context, username, passwordHash string) (User, error) {
	if f.createErr != nil {
		return User{}, f.createErr
	}
	if _, ok := f.byUsername[username]; ok {
		return User{}, ErrUsernameTaken
	}
	u := User{ID: uuid.New(), Username: username, PasswordHash: passwordHash, CreatedAt: time.Now()}
	f.byUsername[username] = u
	return u, nil
}

func (f *fakeRepo) ByUsername(ctx context.Context, username string) (User, error) {
	if f.lookupErr != nil {
		return User{}, f.lookupErr
	}
	u, ok := f.byUsername[username]
	if !ok {
		return User{}, ErrNotFound
	}
	return u, nil
}

func (f *fakeRepo) UsernameByID(ctx context.Context, id uuid.UUID) (string, error) {
	for _, u := range f.byUsername {
		if u.ID == id {
			return u.Username, nil
		}
	}
	return "", ErrNotFound
}

func (f *fakeRepo) ByID(ctx context.Context, id uuid.UUID) (User, error) {
	for _, u := range f.byUsername {
		if u.ID == id {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

// fakeRefreshStore is an in-memory auth.RefreshStore for tests. Tokens are
// keyed by their (fake) hash; the store records revocations for assertions.
type fakeRefreshStore struct {
	byHash        map[string]auth.RefreshToken
	familyRevoked []uuid.UUID
	revokedHashes []string
	consumeErr    error
	byHashErr     error
}

func newFakeRefreshStore() *fakeRefreshStore {
	return &fakeRefreshStore{byHash: make(map[string]auth.RefreshToken)}
}

func (s *fakeRefreshStore) InsertRefreshToken(ctx context.Context, userID, familyID uuid.UUID, hash string, expiresAt time.Time) (uuid.UUID, error) {
	id := uuid.New()
	s.byHash[hash] = auth.RefreshToken{ID: id, UserID: userID, FamilyID: familyID, ExpiresAt: expiresAt}
	return id, nil
}

func (s *fakeRefreshStore) RefreshTokenByHash(ctx context.Context, hash string) (auth.RefreshToken, error) {
	if s.byHashErr != nil {
		return auth.RefreshToken{}, s.byHashErr
	}
	t, ok := s.byHash[hash]
	if !ok {
		return auth.RefreshToken{}, auth.ErrRefreshNotFound
	}
	return t, nil
}

func (s *fakeRefreshStore) ConsumeRefreshToken(ctx context.Context, id, replacedBy uuid.UUID) error {
	if s.consumeErr != nil {
		return s.consumeErr
	}
	for h, t := range s.byHash {
		if t.ID == id {
			t.Consumed = true
			s.byHash[h] = t
		}
	}
	return nil
}

func (s *fakeRefreshStore) RevokeRefreshFamily(ctx context.Context, familyID uuid.UUID) error {
	s.familyRevoked = append(s.familyRevoked, familyID)
	for h, t := range s.byHash {
		if t.FamilyID == familyID {
			delete(s.byHash, h)
		}
	}
	return nil
}

func (s *fakeRefreshStore) RevokeRefreshToken(ctx context.Context, hash string) error {
	s.revokedHashes = append(s.revokedHashes, hash)
	delete(s.byHash, hash)
	return nil
}

// seed inserts a refresh token directly and returns the raw token + its hash,
// so tests can present the raw token to the refresh/logout endpoints.
func (s *fakeRefreshStore) seed(t *testing.T, userID, familyID uuid.UUID, consumed bool, ttl time.Duration) (raw, hash string, id uuid.UUID) {
	t.Helper()
	raw, hash, err := auth.GenerateRefresh()
	if err != nil {
		t.Fatalf("GenerateRefresh: %v", err)
	}
	id = uuid.New()
	s.byHash[hash] = auth.RefreshToken{
		ID: id, UserID: userID, FamilyID: familyID,
		ExpiresAt: time.Now().Add(ttl), Consumed: consumed,
	}
	return raw, hash, id
}

func newTestHandler(t *testing.T, repo Repository) *Handler {
	t.Helper()
	return newTestHandlerWithStore(t, repo, newFakeRefreshStore())
}

func newTestHandlerWithStore(t *testing.T, repo Repository, store auth.RefreshStore) *Handler {
	t.Helper()
	tokens, err := auth.NewTokenService([]byte("test-secret-for-user-handler"), time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	return NewHandler(repo, tokens, store, 24*time.Hour)
}

func doRequest(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) (code string) {
	t.Helper()
	var env struct {
		Error struct {
			Code   string `json:"code"`
			Status int    `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response is not the error envelope: %v (%s)", err, rec.Body.String())
	}
	return env.Error.Code
}

func TestRegister_Created_NoHash(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	rec := doRequest(t, h.register, `{"username":"alice","password":"password123"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}
	if strings.Contains(rec.Body.String(), "password_hash") ||
		strings.Contains(strings.ToLower(rec.Body.String()), "$2a$") {
		t.Fatalf("response must not contain a password hash: %s", rec.Body.String())
	}
	var pub Public
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if pub.Username != "alice" || pub.ID == uuid.Nil {
		t.Fatalf("unexpected user body: %+v", pub)
	}
}

func TestRegister_Duplicate(t *testing.T) {
	repo := newFakeRepo()
	repo.byUsername["alice"] = User{ID: uuid.New(), Username: "alice"}
	h := newTestHandler(t, repo)

	rec := doRequest(t, h.register, `{"username":"alice","password":"password123"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d (%s)", rec.Code, rec.Body.String())
	}
	if decodeError(t, rec) != "conflict" {
		t.Fatalf("expected conflict code, got %q", decodeError(t, rec))
	}
}

func TestRegister_BadJSON(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	rec := doRequest(t, h.register, `{"username":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRegister_Validation(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	cases := []struct {
		name string
		body string
	}{
		{"empty username", `{"username":"","password":"password123"}`},
		{"long username", `{"username":"` + strings.Repeat("a", 65) + `","password":"password123"}`},
		{"short password", `{"username":"bob","password":"short"}`},
		{"empty password", `{"username":"bob","password":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, h.register, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRegister_StoreError(t *testing.T) {
	repo := newFakeRepo()
	repo.createErr = errors.New("boom")
	h := newTestHandler(t, repo)
	rec := doRequest(t, h.register, `{"username":"carol","password":"password123"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("internal error detail must not leak into the response")
	}
}

func TestLogin_OK_IssuesToken(t *testing.T) {
	repo := newFakeRepo()
	h := newTestHandler(t, repo)

	// Register through the handler so the stored hash is real.
	reg := doRequest(t, h.register, `{"username":"dave","password":"password123"}`)
	if reg.Code != http.StatusCreated {
		t.Fatalf("setup register failed: %d", reg.Code)
	}

	rec := doRequest(t, h.login, `{"username":"dave","password":"password123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	if body.Token == "" {
		t.Fatal("expected a token")
	}
	if body.User.Username != "dave" {
		t.Fatalf("unexpected user: %+v", body.User)
	}
	if strings.Contains(rec.Body.String(), "password_hash") {
		t.Fatal("login response must not contain a password hash")
	}
}

func TestLogin_BadCredentials(t *testing.T) {
	repo := newFakeRepo()
	h := newTestHandler(t, repo)
	doRequest(t, h.register, `{"username":"erin","password":"password123"}`)

	cases := []struct {
		name string
		body string
	}{
		{"wrong password", `{"username":"erin","password":"wrongpass1"}`},
		{"unknown user", `{"username":"ghost","password":"password123"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, h.login, tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", rec.Code)
			}
			// Both cases must produce the same generic message.
			if !strings.Contains(rec.Body.String(), "invalid credentials") {
				t.Fatalf("expected generic 'invalid credentials', got %s", rec.Body.String())
			}
		})
	}
}

func TestLogin_Validation(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	rec := doRequest(t, h.login, `{"username":"frank","password":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty password, got %d", rec.Code)
	}
}

// registerUser is a helper that registers a user through the handler and
// returns the stored user (so tests have a real user id for seeding tokens).
func registerUser(t *testing.T, h *Handler, username string) User {
	t.Helper()
	rec := doRequest(t, h.register, `{"username":"`+username+`","password":"password123"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup register %q failed: %d", username, rec.Code)
	}
	var pub Public
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode register body: %v", err)
	}
	return User{ID: pub.ID, Username: pub.Username}
}

func TestLogin_ReturnsRefreshTokenAndExpiry(t *testing.T) {
	repo := newFakeRepo()
	h := newTestHandler(t, repo)
	registerUser(t, h, "dave")

	rec := doRequest(t, h.login, `{"username":"dave","password":"password123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login body: %v", err)
	}
	if body.Token == "" || body.RefreshToken == "" {
		t.Fatalf("expected both access and refresh tokens, got %+v", body)
	}
	if body.ExpiresAt.IsZero() {
		t.Fatal("expected an access-token expiry")
	}
	if !body.ExpiresAt.After(time.Now()) {
		t.Fatalf("expiry should be in the future, got %v", body.ExpiresAt)
	}
}

func TestRefresh_RotatesAndReturnsNewPair(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeRefreshStore()
	h := newTestHandlerWithStore(t, repo, store)
	u := registerUser(t, h, "erin")

	raw, _, _ := store.seed(t, u.ID, uuid.New(), false, time.Hour)

	rec := doRequest(t, h.refreshHandler, `{"refresh_token":"`+raw+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refresh body: %v", err)
	}
	if body.Token == "" || body.RefreshToken == "" {
		t.Fatalf("expected a rotated pair, got %+v", body)
	}
	if body.RefreshToken == raw {
		t.Fatal("refresh must rotate: the new refresh token must differ from the old")
	}
	if body.User.Username != "erin" {
		t.Fatalf("unexpected user: %+v", body.User)
	}
}

func TestRefresh_ReuseRevokesFamily(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeRefreshStore()
	h := newTestHandlerWithStore(t, repo, store)
	u := registerUser(t, h, "fred")
	family := uuid.New()

	// A consumed (already-rotated) token that gets replayed.
	raw, _, _ := store.seed(t, u.ID, family, true, time.Hour)

	rec := doRequest(t, h.refreshHandler, `{"refresh_token":"`+raw+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for reused token, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(store.familyRevoked) != 1 || store.familyRevoked[0] != family {
		t.Fatalf("expected family %v revoked, got %v", family, store.familyRevoked)
	}
}

func TestRefresh_UnknownToken(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	rec := doRequest(t, h.refreshHandler, `{"refresh_token":"does-not-exist"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown token, got %d", rec.Code)
	}
}

func TestRefresh_ExpiredToken(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeRefreshStore()
	h := newTestHandlerWithStore(t, repo, store)
	u := registerUser(t, h, "gwen")

	raw, _, _ := store.seed(t, u.ID, uuid.New(), false, -time.Hour) // already expired

	rec := doRequest(t, h.refreshHandler, `{"refresh_token":"`+raw+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestRefresh_RequiresToken(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	rec := doRequest(t, h.refreshHandler, `{"refresh_token":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty token, got %d", rec.Code)
	}
}

func TestLogout_RevokesToken(t *testing.T) {
	repo := newFakeRepo()
	store := newFakeRefreshStore()
	h := newTestHandlerWithStore(t, repo, store)
	u := registerUser(t, h, "henry")

	raw, hash, _ := store.seed(t, u.ID, uuid.New(), false, time.Hour)

	rec := doRequest(t, h.logout, `{"refresh_token":"`+raw+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(store.revokedHashes) != 1 || store.revokedHashes[0] != hash {
		t.Fatalf("expected token hash revoked, got %v", store.revokedHashes)
	}
	if _, err := store.RefreshTokenByHash(context.Background(), hash); !errors.Is(err, auth.ErrRefreshNotFound) {
		t.Fatalf("expected token gone after logout, err=%v", err)
	}
}

func TestLogout_UnknownTokenStill200(t *testing.T) {
	h := newTestHandler(t, newFakeRepo())
	rec := doRequest(t, h.logout, `{"refresh_token":"never-issued"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout must be idempotent (200 for unknown token), got %d", rec.Code)
	}
}
