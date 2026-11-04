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
