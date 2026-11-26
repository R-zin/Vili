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

type revocation struct{ user, session uuid.UUID }

type fakeRevoker struct{ calls []revocation }

func (f *fakeRevoker) RevokeSession(user, session uuid.UUID) {
	f.calls = append(f.calls, revocation{user, session})
}

func loginAs(t *testing.T, h *Handler, username, password string) loginResponse {
	t.Helper()
	rec := doRequest(t, h.login, `{"username":"`+username+`","password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp
}

func TestLoginTokenCarriesSessionAndVersion(t *testing.T) {
	a := newAcct(t)
	_, _ = a.repo.BumpTokenVersion(context.Background(), a.user.ID) // version 1
	resp := loginAs(t, a.h, "dana", "oldpassword")
	id, err := a.h.tokens.VerifyIdentity(resp.Token)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := a.store.RefreshTokenByHash(context.Background(), auth.HashRefresh(resp.RefreshToken))
	if err != nil {
		t.Fatal(err)
	}
	if id.Version != 1 || id.Session != rt.FamilyID || id.Session == uuid.Nil {
		t.Fatalf("token identity %+v, refresh family %v", id, rt.FamilyID)
	}
}

func TestRevocationHooks(t *testing.T) {
	a := newAcct(t)
	rev := &fakeRevoker{}
	a.h.SetRevoker(rev)

	// Logout ends exactly that session.
	s1 := loginAs(t, a.h, "dana", "oldpassword")
	fam1, _ := a.h.tokens.VerifyIdentity(s1.Token)
	if rec := doRequest(t, a.h.logout, `{"refresh_token":"`+s1.RefreshToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if len(rev.calls) != 1 || rev.calls[0] != (revocation{a.user.ID, fam1.Session}) {
		t.Fatalf("logout revocation: %+v", rev.calls)
	}
	// Logging out an unknown token revokes nothing.
	doRequest(t, a.h.logout, `{"refresh_token":"nope"}`)
	if len(rev.calls) != 1 {
		t.Fatal("unknown token must not trigger a revocation")
	}

	// Revoking one session from the sessions list.
	s2 := loginAs(t, a.h, "dana", "oldpassword")
	fam2, _ := a.h.tokens.VerifyIdentity(s2.Token)
	if rec := a.call(t, a.user.ID, a.h.revokeSession, http.MethodDelete, "", fam2.Session.String()); rec.Code != http.StatusOK {
		t.Fatalf("revoke session: %d", rec.Code)
	}
	if last := rev.calls[len(rev.calls)-1]; last != (revocation{a.user.ID, fam2.Session}) {
		t.Fatalf("session revocation: %+v", last)
	}

	// "Sign out everywhere" bumps the version and revokes all sessions.
	before, _ := a.repo.ByID(context.Background(), a.user.ID)
	if rec := a.call(t, a.user.ID, a.h.revokeAllSessions, http.MethodDelete, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke all: %d", rec.Code)
	}
	after, _ := a.repo.ByID(context.Background(), a.user.ID)
	if after.TokenVersion != before.TokenVersion+1 {
		t.Fatalf("version not bumped: %d -> %d", before.TokenVersion, after.TokenVersion)
	}
	if last := rev.calls[len(rev.calls)-1]; last != (revocation{a.user.ID, uuid.Nil}) {
		t.Fatalf("revoke-all revocation: %+v", last)
	}

	// Password change: everything revoked, the returned pair is at the new version.
	rec := a.call(t, a.user.ID, a.h.changePassword, http.MethodPost, `{"current_password":"oldpassword","new_password":"brandnewpass"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body.String())
	}
	var resp loginResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	id, _ := a.h.tokens.VerifyIdentity(resp.Token)
	now, _ := a.repo.ByID(context.Background(), a.user.ID)
	if id.Version != now.TokenVersion || now.TokenVersion != after.TokenVersion+1 {
		t.Fatalf("new token version %d, stored %d", id.Version, now.TokenVersion)
	}
	if last := rev.calls[len(rev.calls)-1]; last != (revocation{a.user.ID, uuid.Nil}) {
		t.Fatalf("password-change revocation: %+v", last)
	}

	// Account deletion.
	n := len(rev.calls)
	if rec := a.call(t, a.user.ID, a.h.deleteAccount, http.MethodDelete, `{"password":"brandnewpass"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if len(rev.calls) != n+1 || rev.calls[n] != (revocation{a.user.ID, uuid.Nil}) {
		t.Fatalf("delete revocation: %+v", rev.calls)
	}
}

func TestRefreshReuseRevokesSessionTokens(t *testing.T) {
	repo, store := newFakeRepo(), newFakeRefreshStore()
	h := newTestHandlerWithStore(t, repo, store)
	rev := &fakeRevoker{}
	h.SetRevoker(rev)
	hash, _ := auth.HashPassword("password123")
	u, _ := repo.Create(context.Background(), "erin", hash)
	family := uuid.New()
	raw, _, _ := store.seed(t, u.ID, family, true, time.Hour) // already consumed → reuse
	rec := httptest.NewRecorder()
	h.refreshHandler(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"refresh_token":"`+raw+`"}`)))
	if rec.Code != http.StatusUnauthorized || len(rev.calls) != 1 || rev.calls[0] != (revocation{u.ID, family}) {
		t.Fatalf("reuse: %d %+v", rec.Code, rev.calls)
	}
}
