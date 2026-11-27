package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/R-zin/vili/internal/config"
)

// TestOpenPingMigrate is an OPTIONAL real-Postgres integration test. It only
// runs when RUN_DB_TESTS=1 and TEST_DATABASE_URL point at a scratch database.
// The unit-test suite never requires a live database.
func TestOpenPingMigrate(t *testing.T) {
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
			MaxOpenConns:    5,
			MaxIdleConns:    5,
			ConnMaxLifetime: time.Minute,
		},
	}

	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Migrations are idempotent: run twice.
	for i := 0; i < 2; i++ {
		if _, err := Migrate(context.Background(), db); err != nil {
			t.Fatalf("Migrate (run %d): %v", i+1, err)
		}
	}
}

// TestPurge verifies the janitor removes only rows past their grace window.
func TestPurge(t *testing.T) {
	if os.Getenv("RUN_DB_TESTS") != "1" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set RUN_DB_TESTS=1 and TEST_DATABASE_URL to run database integration tests")
	}
	db, err := Open(config.Config{DatabaseURL: os.Getenv("TEST_DATABASE_URL"),
		DB: config.DB{MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	name := fmt.Sprintf("purge_%d", time.Now().UnixNano())
	var uid, rid string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users (id, username, password_hash) VALUES (gen_random_uuid(), $1, 'x') RETURNING id`, name).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		`INSERT INTO rooms (id, name, created_by) VALUES (gen_random_uuid(), $1, $2) RETURNING id`, name, uid).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	for i, ago := range []string{"2 days", "1 hour"} { // long-expired, recently expired
		if _, err := db.ExecContext(ctx, `INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at)
			VALUES (gen_random_uuid(), $1, gen_random_uuid(), $2, now() - $3::interval)`, uid, fmt.Sprintf("%s_%d", name, i), ago); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at)
		VALUES (gen_random_uuid(), $1, gen_random_uuid(), $2, now() + interval '1 day')`, uid, name+"_live"); err != nil {
		t.Fatal(err)
	}
	for i, ago := range []string{"30 days", "1 day"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO room_invites (id, room_id, created_by, code_hash, expires_at)
			VALUES (gen_random_uuid(), $1, $2, $3, now() - $4::interval)`, rid, uid, fmt.Sprintf("%s_i%d", name, i), ago); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := Purge(ctx, db); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	count := func(q string) (n int) {
		if err := db.QueryRowContext(ctx, q, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	if n := count(`SELECT count(*) FROM refresh_tokens WHERE user_id = $1`); n != 2 {
		t.Fatalf("expected recently-expired + live tokens to remain, got %d", n)
	}
	if n := count(`SELECT count(*) FROM room_invites WHERE created_by = $1`); n != 1 {
		t.Fatalf("expected only the recently-expired invite to remain, got %d", n)
	}
	// Idempotent.
	if tk, iv, err := Purge(ctx, db); err != nil || tk != 0 || iv != 0 {
		t.Fatalf("second purge should be a no-op: %d %d %v", tk, iv, err)
	}
}

// TestMigrate_ConcurrentStartups runs migrations from several "instances" at
// once against a brand-new database: all must succeed, and each migration must
// be applied exactly once.
func TestMigrate_ConcurrentStartups(t *testing.T) {
	if os.Getenv("RUN_DB_TESTS") != "1" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set RUN_DB_TESTS=1 and TEST_DATABASE_URL to run database integration tests")
	}
	base := os.Getenv("TEST_DATABASE_URL")
	admin, err := Open(config.Config{DatabaseURL: base, DB: config.DB{MaxOpenConns: 2, MaxIdleConns: 2, ConnMaxLifetime: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("vili_migrate_race_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`) })
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	const instances = 4
	var wg sync.WaitGroup
	errs := make(chan error, instances)
	applied := make(chan int, instances)
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(config.Config{DatabaseURL: u.String(), DB: config.DB{MaxOpenConns: 3, MaxIdleConns: 3, ConnMaxLifetime: time.Minute}})
			if err != nil {
				errs <- err
				return
			}
			defer db.Close()
			ran, err := Migrate(context.Background(), db)
			if err != nil {
				errs <- err
				return
			}
			applied <- len(ran)
		}()
	}
	wg.Wait()
	close(errs)
	close(applied)
	for err := range errs {
		t.Fatalf("concurrent migrate failed: %v", err)
	}
	total, winners := 0, 0
	for n := range applied {
		total += n
		if n > 0 {
			winners++
		}
	}
	names, _ := migrationNames()
	if winners != 1 || total != len(names) {
		t.Fatalf("each migration must run exactly once: winners=%d applied=%d want %d", winners, total, len(names))
	}
}
