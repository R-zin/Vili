package room_test

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

func TestRoomRepository_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	userRepo := user.NewPostgresRepository(db)
	roomRepo := room.NewPostgresRepository(db)

	u1, err := userRepo.Create(ctx, fmt.Sprintf("u_room1_%d", time.Now().UnixNano()), "dummyhash")
	if err != nil {
		t.Fatalf("create user 1: %v", err)
	}
	u2, err := userRepo.Create(ctx, fmt.Sprintf("u_room2_%d", time.Now().UnixNano()), "dummyhash")
	if err != nil {
		t.Fatalf("create user 2: %v", err)
	}

	roomName := fmt.Sprintf("room_%d", time.Now().UnixNano())
	r, err := roomRepo.Create(ctx, u1.ID, roomName, "test room description")
	if err != nil {
		t.Fatalf("roomRepo.Create: %v", err)
	}
	if r.ID == uuid.Nil || r.Name != roomName || r.CreatedBy != u1.ID {
		t.Fatalf("unexpected room: %+v", r)
	}

	// Duplicate room name must fail with ErrNameTaken
	_, err = roomRepo.Create(ctx, u2.ID, roomName, "duplicate name")
	if !errors.Is(err, room.ErrNameTaken) {
		t.Fatalf("expected ErrNameTaken on duplicate room name, got %v", err)
	}

	// ByID
	found, err := roomRepo.ByID(ctx, r.ID)
	if err != nil || found.ID != r.ID || found.Name != roomName {
		t.Fatalf("roomRepo.ByID: %v, got %+v", err, found)
	}

	// List
	list, err := roomRepo.List(ctx)
	if err != nil {
		t.Fatalf("roomRepo.List: %v", err)
	}
	var foundInList bool
	for _, item := range list {
		if item.ID == r.ID {
			foundInList = true
			break
		}
	}
	if !foundInList {
		t.Fatalf("created room not found in room list")
	}

	// AddMember
	if err := roomRepo.AddMember(ctx, r.ID, u2.ID, room.RoleMember); err != nil {
		t.Fatalf("roomRepo.AddMember: %v", err)
	}

	// AddMember duplicate
	err = roomRepo.AddMember(ctx, r.ID, u2.ID, room.RoleMember)
	if !errors.Is(err, room.ErrAlreadyMember) {
		t.Fatalf("expected ErrAlreadyMember on duplicate add, got %v", err)
	}

	// RemoveMember
	if err := roomRepo.RemoveMember(ctx, r.ID, u2.ID); err != nil {
		t.Fatalf("roomRepo.RemoveMember: %v", err)
	}

	// Remove non-member
	err = roomRepo.RemoveMember(ctx, r.ID, u2.ID)
	if !errors.Is(err, room.ErrNotMember) {
		t.Fatalf("expected ErrNotMember when removing already removed member, got %v", err)
	}
}
