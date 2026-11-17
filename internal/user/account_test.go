package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
)

type acctFixture struct {
	h     *Handler
	repo  *fakeRepo
	store *fakeRefreshStore
	user  User
}

func newAcct(t *testing.T) *acctFixture {
	t.Helper()
	repo, store := newFakeRepo(), newFakeRefreshStore()
	h := newTestHandlerWithStore(t, repo, store)
	hash, err := auth.HashPassword("oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := repo.Create(context.Background(), "dana", hash)
	return &acctFixture{h: h, repo: repo, store: store, user: u}
}

func (a *acctFixture) call(t *testing.T, as uuid.UUID, fn http.HandlerFunc, method, body string, pathID string) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := a.h.tokens.Issue(as)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	rec := httptest.NewRecorder()
	a.h.tokens.Middleware(fn).ServeHTTP(rec, req)
	return rec
}

func TestMe(t *testing.T) {
	a := newAcct(t)
	rec := a.call(t, a.user.ID, a.h.me, http.MethodGet, "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dana"`) || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}
	if rec := a.call(t, uuid.New(), a.h.me, http.MethodGet, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted account /me: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.h.me(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no auth ctx: %d", rec.Code)
	}
}

func TestChangePassword(t *testing.T) {
	a := newAcct(t)
	// Two existing sessions, which a password change must end.
	a.store.seed(t, a.user.ID, uuid.New(), false, time.Hour)
	a.store.seed(t, a.user.ID, uuid.New(), false, time.Hour)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad json", `{`, http.StatusBadRequest},
		{"too short", `{"current_password":"oldpassword","new_password":"short"}`, http.StatusBadRequest},
		{"too long", `{"current_password":"oldpassword","new_password":"` + strings.Repeat("x", 73) + `"}`, http.StatusBadRequest},
		{"unchanged", `{"current_password":"oldpassword","new_password":"oldpassword"}`, http.StatusBadRequest},
		{"wrong current", `{"current_password":"nope-nope","new_password":"brandnewpass"}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := a.call(t, a.user.ID, a.h.changePassword, http.MethodPost, tc.body, ""); rec.Code != tc.want {
				t.Fatalf("got %d want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if len(a.store.byHash) != 2 {
				t.Fatal("failed attempts must not revoke sessions")
			}
		})
	}

	rec := a.call(t, a.user.ID, a.h.changePassword, http.MethodPost, `{"current_password":"oldpassword","new_password":"brandnewpass"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Token == "" || resp.RefreshToken == "" {
		t.Fatalf("expected a fresh pair: %+v", resp)
	}
	stored, _ := a.repo.ByUsername(context.Background(), "dana")
	if auth.CompareHashAndPassword(stored.PasswordHash, "brandnewpass") != nil {
		t.Fatal("password not updated")
	}
	if len(a.store.byHash) != 1 {
		t.Fatalf("old sessions must be revoked, only the new one kept: %d", len(a.store.byHash))
	}
	if _, ok := a.store.byHash[auth.HashRefresh(resp.RefreshToken)]; !ok {
		t.Fatal("the new refresh token must be the survivor")
	}
	if rec := a.call(t, uuid.New(), a.h.changePassword, http.MethodPost, `{"current_password":"oldpassword","new_password":"brandnewpass"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown account: %d", rec.Code)
	}
}

func TestDeleteAccount(t *testing.T) {
	a := newAcct(t)
	if rec := a.call(t, a.user.ID, a.h.deleteAccount, http.MethodDelete, `{`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec := a.call(t, a.user.ID, a.h.deleteAccount, http.MethodDelete, `{"password":"wrong"}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	if _, err := a.repo.ByID(context.Background(), a.user.ID); err != nil {
		t.Fatal("account must survive a wrong password")
	}
	if rec := a.call(t, a.user.ID, a.h.deleteAccount, http.MethodDelete, `{"password":"oldpassword"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, err := a.repo.ByID(context.Background(), a.user.ID); err != ErrNotFound {
		t.Fatal("account still exists")
	}
}

func TestSessions(t *testing.T) {
	a := newAcct(t)
	f1, f2, foreign := uuid.New(), uuid.New(), uuid.New()
	a.store.seed(t, a.user.ID, f1, false, time.Hour)
	a.store.seed(t, a.user.ID, f2, false, time.Hour)
	a.store.seed(t, uuid.New(), foreign, false, time.Hour)

	rec := a.call(t, a.user.ID, a.h.listSessions, http.MethodGet, "", "")
	var got []auth.Session
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || len(got) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	if rec := a.call(t, a.user.ID, a.h.revokeSession, http.MethodDelete, "", "not-a-uuid"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
	// Another user's session id looks exactly like a missing one.
	if rec := a.call(t, a.user.ID, a.h.revokeSession, http.MethodDelete, "", foreign.String()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign session: %d", rec.Code)
	}
	if rec := a.call(t, a.user.ID, a.h.revokeSession, http.MethodDelete, "", f1.String()); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := a.call(t, a.user.ID, a.h.revokeAllSessions, http.MethodDelete, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke all: %d", rec.Code)
	}
	if len(a.store.byHash) != 1 {
		t.Fatalf("only the foreign user's token should remain, got %d", len(a.store.byHash))
	}
}

func TestAccountRoutesRequireAuth(t *testing.T) {
	a := newAcct(t)
	mux := http.NewServeMux()
	a.h.RegisterRoutes(mux, nil, a.h.tokens.Require)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/v1/me"}, {"POST", "/v1/me/password"}, {"DELETE", "/v1/me"},
		{"GET", "/v1/me/sessions"}, {"DELETE", "/v1/me/sessions"}, {"DELETE", "/v1/me/sessions/" + uuid.NewString()},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: %d", rt.method, rt.path, rec.Code)
		}
	}
}
