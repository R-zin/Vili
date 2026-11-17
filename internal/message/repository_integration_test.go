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

	r, err := roomRepo.Create(ctx, u.ID, fmt.Sprintf("room_msg_%d", time.Now().UnixNano()), "", false)
	if err != nil {
		t.Fatalf("roomRepo.Create: %v", err)
	}

	// Creating a room makes the creator an owner-member atomically.
	isMem, err := msgRepo.IsMember(ctx, r.ID, u.ID)
	if err != nil || !isMem {
		t.Fatalf("expected creator IsMember=true, got mem=%v, err=%v", isMem, err)
	}
	if role, err := msgRepo.MemberRole(ctx, r.ID, u.ID); err != nil || role != "owner" {
		t.Fatalf("MemberRole = %q, %v", role, err)
	}
	if _, err := msgRepo.MemberRole(ctx, r.ID, uuid.New()); !errors.Is(err, message.ErrNotMember) {
		t.Fatalf("MemberRole non-member: %v", err)
	}
	if isMem, _ := msgRepo.IsMember(ctx, r.ID, uuid.New()); isMem {
		t.Fatal("random user must not be a member")
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

func TestMessageRepository_EditDeleteReplySearch(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	userRepo := user.NewPostgresRepository(db)
	roomRepo := room.NewPostgresRepository(db)
	repo := message.NewPostgresRepository(db)
	stamp := time.Now().UnixNano()

	alice, _ := userRepo.Create(ctx, fmt.Sprintf("al_%d", stamp), "h")
	bob, _ := userRepo.Create(ctx, fmt.Sprintf("bo_%d", stamp), "h")
	eve, _ := userRepo.Create(ctx, fmt.Sprintf("ev_%d", stamp), "h") // never joins
	r, _ := roomRepo.Create(ctx, alice.ID, fmt.Sprintf("rs_%d", stamp), "", false)
	if err := roomRepo.AddMember(ctx, r.ID, bob.ID, room.RoleMember); err != nil {
		t.Fatal(err)
	}
	other, _ := roomRepo.Create(ctx, bob.ID, fmt.Sprintf("rs2_%d", stamp), "", false)

	// Mentions are narrowed to real members and de-duplicated.
	m1 := message.Message{RoomID: r.ID, UserID: alice.ID, Content: "kubernetes rollout done",
		Mentions: []string{bob.Username, eve.Username, "ghost", bob.Username}}
	if err := repo.Create(ctx, &m1); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(m1.Mentions) != 1 || m1.Mentions[0] != bob.Username {
		t.Fatalf("mentions not resolved to members: %v", m1.Mentions)
	}
	got, err := repo.ByID(ctx, m1.ID)
	if err != nil || len(got.Mentions) != 1 || got.Deleted || got.EditedAt != nil {
		t.Fatalf("ByID: %v %+v", err, got)
	}
	if _, err := repo.ByID(ctx, uuid.New()); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("ByID unknown: %v", err)
	}

	// Replies must target a message in the same room.
	reply := message.Message{RoomID: r.ID, UserID: bob.ID, Content: "thanks", ReplyTo: &m1.ID}
	if err := repo.Create(ctx, &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if h, _ := repo.ListByRoom(ctx, r.ID, 10, nil); h[len(h)-1].ReplyTo == nil || *h[len(h)-1].ReplyTo != m1.ID {
		t.Fatalf("reply_to not persisted: %+v", h)
	}
	foreign := message.Message{RoomID: other.ID, UserID: bob.ID, Content: "x", ReplyTo: &m1.ID}
	if err := repo.Create(ctx, &foreign); !errors.Is(err, message.ErrReplyNotFound) {
		t.Fatalf("cross-room reply: %v", err)
	}
	ghost := uuid.New()
	if err := repo.Create(ctx, &message.Message{RoomID: r.ID, UserID: bob.ID, Content: "x", ReplyTo: &ghost}); !errors.Is(err, message.ErrReplyNotFound) {
		t.Fatalf("unknown reply: %v", err)
	}

	// Search: full-text, scoped to rooms the caller belongs to.
	hits, err := repo.Search(ctx, bob.ID, "kubernetes", nil, 10)
	if err != nil || len(hits) != 1 || hits[0].ID != m1.ID || hits[0].RoomName == "" {
		t.Fatalf("search: %v %+v", err, hits)
	}
	if hits, _ := repo.Search(ctx, eve.ID, "kubernetes", nil, 10); len(hits) != 0 {
		t.Fatalf("non-member search leaked %d results", len(hits))
	}
	if hits, _ := repo.Search(ctx, bob.ID, "kubernetes", &other.ID, 10); len(hits) != 0 {
		t.Fatalf("room filter ignored: %d", len(hits))
	}
	if hits, _ := repo.Search(ctx, bob.ID, "kubernetes -rollout", nil, 10); len(hits) != 0 {
		t.Fatalf("websearch negation ignored: %d", len(hits))
	}
	if _, err := repo.Search(ctx, bob.ID, `"unbalanced quote OR (`, nil, 10); err != nil {
		t.Fatalf("malformed query must not error: %v", err)
	}

	// Edit.
	edited, err := repo.Edit(ctx, m1.ID, "helm rollout done "+"@"+eve.Username, []string{eve.Username, bob.Username})
	if err != nil || edited.EditedAt == nil || edited.Content != "helm rollout done @"+eve.Username || len(edited.Mentions) != 1 {
		t.Fatalf("edit: %v %+v", err, edited)
	}
	if hits, _ := repo.Search(ctx, bob.ID, "kubernetes", nil, 10); len(hits) != 0 {
		t.Fatal("search index must follow edits")
	}
	if hits, _ := repo.Search(ctx, bob.ID, "helm", nil, 10); len(hits) != 1 {
		t.Fatal("edited content must be searchable")
	}

	// Delete tombstones: content gone, row kept for replies, unsearchable.
	if err := repo.Delete(ctx, m1.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.Delete(ctx, m1.ID); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("double delete: %v", err)
	}
	if _, err := repo.Edit(ctx, m1.ID, "zombie", nil); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("edit deleted: %v", err)
	}
	if _, err := repo.Edit(ctx, uuid.New(), "x", nil); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("edit unknown: %v", err)
	}
	tomb, _ := repo.ByID(ctx, m1.ID)
	if !tomb.Deleted || tomb.Content != "" || len(tomb.Mentions) != 0 {
		t.Fatalf("tombstone: %+v", tomb)
	}
	if hits, _ := repo.Search(ctx, bob.ID, "helm", nil, 10); len(hits) != 0 {
		t.Fatal("deleted message must not be searchable")
	}
	hist, _ := repo.ListByRoom(ctx, r.ID, 10, nil)
	if len(hist) != 2 || !hist[0].Deleted {
		t.Fatalf("history must keep the tombstone: %+v", hist)
	}
}
