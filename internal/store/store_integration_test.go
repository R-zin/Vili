package store

import (
	"context"
	"os"
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
