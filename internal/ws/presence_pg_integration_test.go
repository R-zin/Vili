package ws_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/config"
	"github.com/R-zin/vili/internal/store"
	"github.com/R-zin/vili/internal/ws"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("RUN_DB_TESTS") != "1" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set RUN_DB_TESTS=1 and TEST_DATABASE_URL to run database integration tests")
	}
	db, err := store.Open(config.Config{DatabaseURL: os.Getenv("TEST_DATABASE_URL"),
		DB: config.DB{MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPGPresence_ClusterWide(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	mkUser := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := db.Exec(`INSERT INTO users (id, username, password_hash) VALUES ($1, $2, 'x')`, id, fmt.Sprintf("%s_%d", name, stamp)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	alice, bob := mkUser("alice"), mkUser("bob")
	room := uuid.New()
	if _, err := db.Exec(`INSERT INTO rooms (id, name, created_by) VALUES ($1, $2, $3)`, room, fmt.Sprintf("pres_%d", stamp), alice); err != nil {
		t.Fatal(err)
	}
	an, bn := fmt.Sprintf("alice_%d", stamp), fmt.Sprintf("bob_%d", stamp)

	i1 := ws.NewPGPresence(db, fmt.Sprintf("i1-%d", stamp), nil)
	var snapshot []ws.PresenceEntry
	i2 := ws.NewPGPresence(db, fmt.Sprintf("i2-%d", stamp), func() []ws.PresenceEntry { return snapshot })
	for _, p := range []*ws.PGPresence{i1, i2} {
		if err := p.Register(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// alice on instance 1 (two tabs), bob on instance 2.
	_ = i1.Add(ctx, room, alice, an)
	_ = i1.Add(ctx, room, alice, an)
	_ = i2.Add(ctx, room, bob, bn)
	online, err := i2.Online(ctx, room)
	if err != nil || len(online) != 2 {
		t.Fatalf("cluster-wide online: %v %v", online, err)
	}
	// Closing one of alice's two tabs keeps her online; closing both removes her.
	_ = i1.Remove(ctx, room, alice)
	if online, _ := i1.Online(ctx, room); len(online) != 2 {
		t.Fatalf("one tab still open: %v", online)
	}
	_ = i1.Remove(ctx, room, alice)
	_ = i1.Remove(ctx, room, alice) // extra remove is harmless
	if online, _ := i1.Online(ctx, room); len(online) != 1 || online[0] != bn {
		t.Fatalf("after alice left: %v", online)
	}

	// A crashed instance (no heartbeat) stops counting without anyone cleaning up.
	_ = i1.Add(ctx, room, alice, an)
	if _, err := db.Exec(`UPDATE ws_instances SET heartbeat_at = now() - interval '2 minutes' WHERE id = $1`, fmt.Sprintf("i1-%d", stamp)); err != nil {
		t.Fatal(err)
	}
	if online, _ := i2.Online(ctx, room); len(online) != 1 || online[0] != bn {
		t.Fatalf("stale instance must be ignored: %v", online)
	}

	// If an instance's row disappears (janitor), its heartbeat re-registers and
	// restores presence from the hub snapshot.
	snapshot = []ws.PresenceEntry{{Room: room, User: bob, Username: bn, Conns: 3}}
	if _, err := db.Exec(`DELETE FROM ws_instances WHERE id = $1`, fmt.Sprintf("i2-%d", stamp)); err != nil {
		t.Fatal(err)
	}
	if online, _ := i1.Online(ctx, room); len(online) != 0 {
		t.Fatalf("instance gone: %v", online)
	}
	hbCtx, cancel := context.WithCancel(ctx)
	go i2.Run(hbCtx, 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	cancel()
	if online, _ := i1.Online(ctx, room); len(online) != 1 || online[0] != bn {
		t.Fatalf("presence not restored after re-registration: %v", online)
	}

	// Clean shutdown removes an instance's users immediately.
	if err := i2.Deregister(ctx); err != nil {
		t.Fatal(err)
	}
	if online, _ := i1.Online(ctx, room); len(online) != 0 {
		t.Fatalf("after deregister: %v", online)
	}
}
