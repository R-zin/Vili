package bus_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/bus"
	"github.com/R-zin/vili/internal/config"
	"github.com/R-zin/vili/internal/event"
	"github.com/R-zin/vili/internal/store"
)

func openDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	if os.Getenv("RUN_DB_TESTS") != "1" || os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("set RUN_DB_TESTS=1 and TEST_DATABASE_URL to run database integration tests")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	db, err := store.Open(config.Config{DatabaseURL: dsn, DB: config.DB{MaxOpenConns: 5, MaxIdleConns: 5, ConnMaxLifetime: time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dsn
}

// recorder collects delivered messages safely.
type recorder struct {
	mu   sync.Mutex
	msgs []bus.Message
	ch   chan bus.Message
}

func newRecorder() *recorder { return &recorder{ch: make(chan bus.Message, 64)} }

func (r *recorder) handle(m bus.Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	r.ch <- m
}

func (r *recorder) next(t *testing.T) bus.Message {
	t.Helper()
	select {
	case m := <-r.ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a bus message")
		return bus.Message{}
	}
}

func (r *recorder) none(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("unexpected message %+v", m)
	case <-time.After(within):
	}
}

func startBus(t *testing.T, db *sql.DB, dsn, name string) (*bus.Postgres, *recorder) {
	t.Helper()
	b := bus.NewPostgres(db, dsn, name+"-"+uuid.NewString()[:6])
	rec := newRecorder()
	b.Subscribe(rec.handle)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go b.Run(ctx, ready)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("bus listener did not connect")
	}
	return b, rec
}

func TestPostgresBus_CrossInstanceDelivery(t *testing.T) {
	db, dsn := openDB(t)
	a, recA := startBus(t, db, dsn, "a")
	_, recB := startBus(t, db, dsn, "b")

	room := uuid.New()
	ev := event.NewTyping(room, "alice")
	if err := a.Publish(context.Background(), bus.Message{Kind: bus.KindRoomEvent, Room: room, Event: &ev}); err != nil {
		t.Fatal(err)
	}
	// The publisher delivers locally exactly once (no echo from NOTIFY) ...
	if m := recA.next(t); m.Kind != bus.KindRoomEvent || m.Room != room {
		t.Fatalf("local delivery: %+v", m)
	}
	// ... and the peer receives it intact.
	m := recB.next(t)
	if m.Kind != bus.KindRoomEvent || m.Room != room || m.Event == nil || m.Event.Type != event.Typing {
		t.Fatalf("remote delivery: %+v", m)
	}
	recA.none(t, 300*time.Millisecond)
	if p, _ := a.Stats(); p != 1 {
		t.Fatalf("published = %d", p)
	}

	// Payloads over the NOTIFY limit go through the outbox.
	big := strings.Repeat("\u0001", 4000) // JSON-escapes to ~24 KB
	bigEv, _ := event.NewMessage(room, map[string]string{"content": big})
	if err := a.Publish(context.Background(), bus.Message{Kind: bus.KindRoomEvent, Room: room, Event: &bigEv}); err != nil {
		t.Fatalf("large publish: %v", err)
	}
	recA.next(t)
	got := recB.next(t)
	if got.Event == nil || !strings.Contains(string(got.Event.Payload), `\u0001`) || len(got.Event.Payload) < 20000 {
		t.Fatalf("large message lost or truncated (%d bytes)", len(got.Event.Payload))
	}
	var outbox int
	_ = db.QueryRow(`SELECT count(*) FROM bus_outbox`).Scan(&outbox)
	if outbox == 0 {
		t.Fatal("large payload should have used the outbox")
	}
}

func TestPostgresBus_ReconnectsAfterConnectionLoss(t *testing.T) {
	db, dsn := openDB(t)
	a, _ := startBus(t, db, dsn, "a")
	b, recB := startBus(t, db, dsn, "b")

	// Kill every LISTEN connection; both listeners must come back by themselves.
	if _, err := db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE query LIKE 'LISTEN%' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		if b.Connected() && a.Connected() {
			// Connected() flips back only after a fresh LISTEN; give the
			// terminate a moment to be noticed first.
			var n int
			_ = db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'LISTEN%'`).Scan(&n)
			if n >= 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("listeners did not reconnect")
		}
	}
	m := bus.Message{Kind: bus.KindCloseRoom, Room: uuid.New()}
	if err := a.Publish(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got := recB.next(t); got.Room != m.Room {
		t.Fatalf("after reconnect: %+v", got)
	}
}
