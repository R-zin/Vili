package message_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/config"
	"github.com/R-zin/vili/internal/message"
	"github.com/R-zin/vili/internal/room"
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

func TestMessageRepository_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	userRepo := user.NewPostgresRepository(db)
	roomRepo := room.NewPostgresRepository(db)
	msgRepo := message.NewPostgresRepository(db)

	uname := fmt.Sprintf("u_msg_%d", time.Now().UnixNano())
	u, err := userRepo.Create(ctx, uname, "dummyhash")
	if err != nil {
		t.Fatalf("userRepo.Create: %v", err)
	}

	r, err := roomRepo.Create(ctx, u.ID, fmt.Sprintf("room_msg_%d", time.Now().UnixNano()), "")
	if err != nil {
		t.Fatalf("roomRepo.Create: %v", err)
	}

	// Membership check before adding
	isMem, err := msgRepo.IsMember(ctx, r.ID, u.ID)
	if err != nil || isMem {
		t.Fatalf("expected IsMember=false before AddMember, got mem=%v, err=%v", isMem, err)
	}

	if err := roomRepo.AddMember(ctx, r.ID, u.ID, room.RoleOwner); err != nil {
		t.Fatalf("roomRepo.AddMember: %v", err)
	}

	isMem, err = msgRepo.IsMember(ctx, r.ID, u.ID)
	if err != nil || !isMem {
		t.Fatalf("expected IsMember=true after AddMember, got mem=%v, err=%v", isMem, err)
	}

	// Create message
	m1 := message.Message{
		RoomID:  r.ID,
		UserID:  u.ID,
		Content: "hello from postgres integration test",
		Type:    message.TypeText,
	}
	if err := msgRepo.Create(ctx, &m1); err != nil {
		t.Fatalf("msgRepo.Create m1: %v", err)
	}
	if m1.ID == uuid.Nil || m1.Username != uname || m1.CreatedAt.IsZero() {
		t.Fatalf("unexpected message after create: %+v", m1)
	}

	// Create message to non-existent room must fail with ErrRoomNotFound
	missingRoomMsg := message.Message{
		RoomID:  uuid.New(),
		UserID:  u.ID,
		Content: "nowhere",
	}
	err = msgRepo.Create(ctx, &missingRoomMsg)
	if !errors.Is(err, message.ErrRoomNotFound) {
		t.Fatalf("expected ErrRoomNotFound for missing room, got %v", err)
	}

	// Insert second message
	m2 := message.Message{
		RoomID:    r.ID,
		UserID:    u.ID,
		Content:   "second message",
		Type:      message.TypeCode,
		CreatedAt: m1.CreatedAt.Add(time.Second),
	}
	if err := msgRepo.Create(ctx, &m2); err != nil {
		t.Fatalf("msgRepo.Create m2: %v", err)
	}

	// ListByRoom: chronological order (oldest first)
	history, err := msgRepo.ListByRoom(ctx, r.ID, 10, nil)
	if err != nil {
		t.Fatalf("ListByRoom: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(history))
	}
	if history[0].ID != m1.ID || history[1].ID != m2.ID {
		t.Fatalf("messages not in chronological order: %+v", history)
	}

	// Cursor pagination: before m2 must only return m1
	beforeTime := m2.CreatedAt
	pagedHistory, err := msgRepo.ListByRoom(ctx, r.ID, 10, &beforeTime)
	if err != nil {
		t.Fatalf("ListByRoom with before: %v", err)
	}
	if len(pagedHistory) != 1 || pagedHistory[0].ID != m1.ID {
		t.Fatalf("expected only m1 with before cursor, got %+v", pagedHistory)
	}
}
