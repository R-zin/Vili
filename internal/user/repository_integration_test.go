package user_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/config"
	"github.com/R-zin/vili/internal/store"
	"github.com/R-zin/vili/internal/user"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("RUN_DB_TESTS") != "1" {
		t.Skip("set RUN_DB_TESTS=1 to run database integration tests")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to a scratch Postgres database")
	}

	cfg := config.Config{
		DatabaseURL: dsn,
		DB: config.DB{
			MaxOpenConns:    10,
			MaxIdleConns:    10,
			ConnMaxLifetime: time.Minute,
		},
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := store.Migrate(context.Background(), db); err != nil {
		db.Close()
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

func TestUserRepository_Integration_CreateAndFind(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := user.NewPostgresRepository(db)
	ctx := context.Background()

	uname := fmt.Sprintf("u_%d", time.Now().UnixNano())
	pwdHash := "$2a$10$dummyhashforuserrepocreateandfind123456789012345678"

	u, err := repo.Create(ctx, uname, pwdHash)
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}
	if u.ID == uuid.Nil || u.Username != uname || u.PasswordHash != pwdHash {
		t.Fatalf("unexpected user created: %+v", u)
	}

	// Duplicate creation must fail with ErrUsernameTaken
	_, err = repo.Create(ctx, uname, pwdHash)
	if !errors.Is(err, user.ErrUsernameTaken) {
		t.Fatalf("expected ErrUsernameTaken on duplicate create, got %v", err)
	}

	// ByID
	found, err := repo.ByID(ctx, u.ID)
	if err != nil || found.ID != u.ID || found.Username != uname {
		t.Fatalf("repo.ByID: %v, got %+v", err, found)
	}

	// ByUsername
	foundByUname, err := repo.ByUsername(ctx, uname)
	if err != nil || foundByUname.ID != u.ID {
		t.Fatalf("repo.ByUsername: %v, got %+v", err, foundByUname)
	}

	// UsernameByID
	name, err := repo.UsernameByID(ctx, u.ID)
	if err != nil || name != uname {
		t.Fatalf("repo.UsernameByID: %v, got %q", err, name)
	}

	// Non-existent user
	_, err = repo.ByID(ctx, uuid.New())
	if !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing ID, got %v", err)
	}
}

func TestUserRepository_Integration_RefreshTokens(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := user.NewPostgresRepository(db)
	ctx := context.Background()

	uname := fmt.Sprintf("u_ref_%d", time.Now().UnixNano())
	u, err := repo.Create(ctx, uname, "dummyhash")
	if err != nil {
		t.Fatalf("repo.Create: %v", err)
	}

	familyID := uuid.New()
	tokenHash := fmt.Sprintf("hash_%d", time.Now().UnixNano())
	expiresAt := time.Now().Add(24 * time.Hour).Truncate(time.Microsecond)

	tokenID, err := repo.InsertRefreshToken(ctx, u.ID, familyID, tokenHash, expiresAt)
	if err != nil {
		t.Fatalf("InsertRefreshToken: %v", err)
	}
	if tokenID == uuid.Nil {
		t.Fatal("expected non-nil token ID")
	}

	// Fetch token by hash
	tok, err := repo.RefreshTokenByHash(ctx, tokenHash)
	if err != nil {
		t.Fatalf("RefreshTokenByHash: %v", err)
	}
	if tok.ID != tokenID || tok.UserID != u.ID || tok.FamilyID != familyID || tok.Consumed {
		t.Fatalf("unexpected token returned: %+v", tok)
	}

	// First consumption succeeds
	replacedBy := uuid.New()
	if err := repo.ConsumeRefreshToken(ctx, tokenID, replacedBy); err != nil {
		t.Fatalf("ConsumeRefreshToken first call: %v", err)
	}

	// Verify consumed flag
	tok, err = repo.RefreshTokenByHash(ctx, tokenHash)
	if err != nil || !tok.Consumed {
		t.Fatalf("expected token to be consumed, got err=%v, tok=%+v", err, tok)
	}

	// Second consumption fails with ErrRefreshAlreadyConsumed
	err = repo.ConsumeRefreshToken(ctx, tokenID, uuid.New())
	if !errors.Is(err, auth.ErrRefreshAlreadyConsumed) {
		t.Fatalf("expected ErrRefreshAlreadyConsumed on replay, got %v", err)
	}

	// Concurrent race condition test on fresh token
	raceTokenHash := fmt.Sprintf("race_%d", time.Now().UnixNano())
	raceTokenID, err := repo.InsertRefreshToken(ctx, u.ID, familyID, raceTokenHash, expiresAt)
	if err != nil {
		t.Fatalf("InsertRefreshToken for race: %v", err)
	}

	const concurrency = 8
	var wg sync.WaitGroup
	var successCount int64
	var raceErrors []error
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.ConsumeRefreshToken(ctx, raceTokenID, uuid.New())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successCount++
			} else {
				raceErrors = append(raceErrors, err)
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 concurrent consume to succeed, got %d", successCount)
	}
	for _, err := range raceErrors {
		if !errors.Is(err, auth.ErrRefreshAlreadyConsumed) {
			t.Errorf("expected ErrRefreshAlreadyConsumed for race losers, got %v", err)
		}
	}

	// RevokeRefreshToken (single token logout)
	if err := repo.RevokeRefreshToken(ctx, tokenHash); err != nil {
		t.Fatalf("RevokeRefreshToken: %v", err)
	}
	_, err = repo.RefreshTokenByHash(ctx, tokenHash)
	if !errors.Is(err, auth.ErrRefreshNotFound) {
		t.Fatalf("expected ErrRefreshNotFound after RevokeRefreshToken, got %v", err)
	}

	// RevokeRefreshFamily (reuse detection revocation)
	if err := repo.RevokeRefreshFamily(ctx, familyID); err != nil {
		t.Fatalf("RevokeRefreshFamily: %v", err)
	}
	_, err = repo.RefreshTokenByHash(ctx, raceTokenHash)
	if !errors.Is(err, auth.ErrRefreshNotFound) {
		t.Fatalf("expected ErrRefreshNotFound after RevokeRefreshFamily, got %v", err)
	}
}

func TestUserRepository_Integration_AccountLifecycle(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	repo := user.NewPostgresRepository(db)

	u, err := repo.Create(ctx, fmt.Sprintf("acct_%d", time.Now().UnixNano()), "hash1")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := repo.Create(ctx, fmt.Sprintf("acct_o_%d", time.Now().UnixNano()), "hash")

	if err := repo.UpdatePassword(ctx, u.ID, "hash2"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if got, _ := repo.ByID(ctx, u.ID); got.PasswordHash != "hash2" {
		t.Fatalf("hash not stored: %q", got.PasswordHash)
	}
	if err := repo.UpdatePassword(ctx, uuid.New(), "x"); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("UpdatePassword unknown: %v", err)
	}

	// Sessions: two families for u (one with a consumed + a live token), one
	// expired family, and one belonging to someone else.
	famA, famB, famExpired, famOther := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exp := time.Now().Add(time.Hour)
	oldA, _ := repo.InsertRefreshToken(ctx, u.ID, famA, "ha1", exp)
	newA, _ := repo.InsertRefreshToken(ctx, u.ID, famA, "ha2", exp)
	if err := repo.ConsumeRefreshToken(ctx, oldA, newA); err != nil {
		t.Fatal(err)
	}
	_, _ = repo.InsertRefreshToken(ctx, u.ID, famB, "hb1", exp)
	_, _ = repo.InsertRefreshToken(ctx, u.ID, famExpired, "he1", time.Now().Add(-time.Hour))
	_, _ = repo.InsertRefreshToken(ctx, other.ID, famOther, "ho1", exp)

	sessions, err := repo.ListSessions(ctx, u.ID)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("ListSessions: %v %+v", err, sessions)
	}
	ids := map[uuid.UUID]bool{sessions[0].ID: true, sessions[1].ID: true}
	if !ids[famA] || !ids[famB] {
		t.Fatalf("expected families A and B, got %+v", sessions)
	}
	if found, err := repo.RevokeSession(ctx, u.ID, famOther); err != nil || found {
		t.Fatalf("must not revoke another user's session: %v %v", found, err)
	}
	if found, err := repo.RevokeSession(ctx, u.ID, famA); err != nil || !found {
		t.Fatalf("revoke A: %v %v", found, err)
	}
	if s, _ := repo.ListSessions(ctx, u.ID); len(s) != 1 || s[0].ID != famB {
		t.Fatalf("after revoke: %+v", s)
	}
	if err := repo.RevokeAllSessions(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := repo.ListSessions(ctx, u.ID); len(s) != 0 {
		t.Fatalf("after revoke all: %+v", s)
	}
	if s, _ := repo.ListSessions(ctx, other.ID); len(s) != 1 {
		t.Fatal("other user's sessions must survive")
	}

	// Delete cascades to the user's tokens, and is not repeatable.
	if err := repo.Delete(ctx, other.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.RefreshTokenByHash(ctx, "ho1"); !errors.Is(err, auth.ErrRefreshNotFound) {
		t.Fatalf("tokens must cascade: %v", err)
	}
	if err := repo.Delete(ctx, other.ID); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestUserRepository_Integration_SessionState(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	repo := user.NewPostgresRepository(db)
	u, err := repo.Create(ctx, fmt.Sprintf("ss_%d", time.Now().UnixNano()), "h")
	if err != nil || u.TokenVersion != 0 {
		t.Fatalf("create: %v %+v", err, u)
	}
	fam := uuid.New()
	if _, err := repo.InsertRefreshToken(ctx, u.ID, fam, fmt.Sprintf("h_%d", time.Now().UnixNano()), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	st, err := repo.SessionState(ctx, u.ID, fam)
	if err != nil || !st.UserExists || st.Version != 0 || !st.SessionLive {
		t.Fatalf("live session: %v %+v", err, st)
	}
	if st, _ := repo.SessionState(ctx, u.ID, uuid.Nil); !st.SessionLive {
		t.Fatal("a token without a session is not session-bound")
	}
	if st, _ := repo.SessionState(ctx, u.ID, uuid.New()); st.SessionLive {
		t.Fatal("unknown session must not be live")
	}
	other, _ := repo.Create(ctx, fmt.Sprintf("ss_o_%d", time.Now().UnixNano()), "h")
	if st, _ := repo.SessionState(ctx, other.ID, fam); st.SessionLive {
		t.Fatal("another user's session must not count")
	}

	v, err := repo.BumpTokenVersion(ctx, u.ID)
	if err != nil || v != 1 {
		t.Fatalf("bump: %d %v", v, err)
	}
	if got, _ := repo.ByUsername(ctx, u.Username); got.TokenVersion != 1 {
		t.Fatalf("ByUsername version: %d", got.TokenVersion)
	}
	if _, err := repo.BumpTokenVersion(ctx, uuid.New()); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("bump unknown: %v", err)
	}

	if err := repo.RevokeRefreshFamily(ctx, fam); err != nil {
		t.Fatal(err)
	}
	if st, _ := repo.SessionState(ctx, u.ID, fam); st.SessionLive || st.Version != 1 {
		t.Fatalf("revoked session: %+v", st)
	}
	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if st, err := repo.SessionState(ctx, u.ID, uuid.Nil); err != nil || st.UserExists {
		t.Fatalf("deleted user: %v %+v", err, st)
	}
}
