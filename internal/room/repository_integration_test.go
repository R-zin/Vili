package room_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
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

func uniq(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()) }

func mkUser(t *testing.T, db *sql.DB, prefix string) user.User {
	t.Helper()
	u, err := user.NewPostgresRepository(db).Create(context.Background(), uniq(prefix), "dummyhash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestRoomRepository_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	roomRepo := room.NewPostgresRepository(db)
	u1, u2 := mkUser(t, db, "u_room1"), mkUser(t, db, "u_room2")

	roomName := uniq("room")
	r, err := roomRepo.Create(ctx, u1.ID, roomName, "test room description", false)
	if err != nil {
		t.Fatalf("roomRepo.Create: %v", err)
	}
	if r.ID == uuid.Nil || r.Name != roomName || r.CreatedBy != u1.ID || r.IsPrivate {
		t.Fatalf("unexpected room: %+v", r)
	}
	// Creation is atomic with the owner membership.
	if role, err := roomRepo.Role(ctx, r.ID, u1.ID); err != nil || role != room.RoleOwner {
		t.Fatalf("creator role = %q, %v", role, err)
	}

	if _, err = roomRepo.Create(ctx, u2.ID, roomName, "dup", false); !errors.Is(err, room.ErrNameTaken) {
		t.Fatalf("expected ErrNameTaken, got %v", err)
	}
	if found, err := roomRepo.ByID(ctx, r.ID); err != nil || found.ID != r.ID {
		t.Fatalf("ByID: %v %+v", err, found)
	}
	if _, err := roomRepo.ByID(ctx, uuid.New()); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("ByID unknown: %v", err)
	}

	list, err := roomRepo.List(ctx, u2.ID, room.ListFilter{Query: roomName[:8], Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var seen bool
	for _, item := range list {
		if item.ID == r.ID {
			seen = true
			if item.Role != "" || item.MemberCount != 1 {
				t.Fatalf("non-member summary wrong: %+v", item)
			}
		}
	}
	if !seen {
		t.Fatal("public room missing from list")
	}
	// LIKE metacharacters in the query match literally.
	if got, _ := roomRepo.List(ctx, u2.ID, room.ListFilter{Query: "%", Limit: 50}); len(got) != 0 {
		t.Fatalf("%% must not be a wildcard, got %d rooms", len(got))
	}

	if err := roomRepo.AddMember(ctx, r.ID, u2.ID, room.RoleMember); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := roomRepo.AddMember(ctx, r.ID, u2.ID, room.RoleMember); !errors.Is(err, room.ErrAlreadyMember) {
		t.Fatalf("expected ErrAlreadyMember, got %v", err)
	}
	if err := roomRepo.AddMember(ctx, uuid.New(), u2.ID, room.RoleMember); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	members, err := roomRepo.Members(ctx, r.ID)
	if err != nil || len(members) != 2 || members[0].Role != room.RoleOwner {
		t.Fatalf("Members: %v %+v", err, members)
	}
	if err := roomRepo.SetRole(ctx, r.ID, u2.ID, room.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if err := roomRepo.SetRole(ctx, r.ID, uuid.New(), room.RoleAdmin); !errors.Is(err, room.ErrNotMember) {
		t.Fatalf("SetRole non-member: %v", err)
	}

	desc, priv := "changed", true
	upd, err := roomRepo.Update(ctx, r.ID, room.UpdateParams{Description: &desc, IsPrivate: &priv})
	if err != nil || upd.Description != "changed" || !upd.IsPrivate || upd.Name != roomName {
		t.Fatalf("Update: %v %+v", err, upd)
	}
	// Now private: hidden from a non-member's list.
	u3 := mkUser(t, db, "u_room3")
	got, _ := roomRepo.List(ctx, u3.ID, room.ListFilter{Query: roomName, Limit: 50})
	if len(got) != 0 {
		t.Fatal("private room visible to non-member")
	}
	if got, _ := roomRepo.List(ctx, u2.ID, room.ListFilter{Query: roomName, MineOnly: true, Limit: 50}); len(got) != 1 {
		t.Fatalf("private room must be visible to member, got %d", len(got))
	}

	// Ownership transfer: when the only owner leaves, the admin inherits.
	if err := roomRepo.RemoveMember(ctx, r.ID, u1.ID); err != nil {
		t.Fatalf("owner leave: %v", err)
	}
	if role, _ := roomRepo.Role(ctx, r.ID, u2.ID); role != room.RoleOwner {
		t.Fatalf("expected ownership transfer, got %q", role)
	}
	if err := roomRepo.RemoveMember(ctx, r.ID, u1.ID); !errors.Is(err, room.ErrNotMember) {
		t.Fatalf("expected ErrNotMember, got %v", err)
	}
	if err := roomRepo.RemoveMember(ctx, uuid.New(), u1.ID); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// Last member leaving deletes the room.
	if err := roomRepo.RemoveMember(ctx, r.ID, u2.ID); err != nil {
		t.Fatalf("last leave: %v", err)
	}
	if _, err := roomRepo.ByID(ctx, r.ID); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("empty room should be deleted: %v", err)
	}
}

func TestRoomRepository_UpdateDeleteConflicts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	repo := room.NewPostgresRepository(db)
	u := mkUser(t, db, "u_upd")
	a, _ := repo.Create(ctx, u.ID, uniq("a"), "", false)
	b, _ := repo.Create(ctx, u.ID, uniq("b"), "", false)
	if _, err := repo.Update(ctx, b.ID, room.UpdateParams{Name: &a.Name}); !errors.Is(err, room.ErrNameTaken) {
		t.Fatalf("rename conflict: %v", err)
	}
	if _, err := repo.Update(ctx, uuid.New(), room.UpdateParams{Name: &a.Name}); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, a.ID); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("double delete: %v", err)
	}
	if err := repo.MarkRead(ctx, b.ID, uuid.New()); !errors.Is(err, room.ErrNotMember) {
		t.Fatalf("mark read non-member: %v", err)
	}
}

func TestRoomRepository_Invites(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	repo := room.NewPostgresRepository(db)
	owner := mkUser(t, db, "u_inv_o")
	r, _ := repo.Create(ctx, owner.ID, uniq("inv"), "", true)

	// max_uses = 2 under a concurrent stampede of 8 distinct users: exactly 2 win.
	hash := room.HashInviteCode(uniq("code"))
	inv, err := repo.CreateInvite(ctx, r.ID, owner.ID, hash, time.Now().Add(time.Hour), 2)
	if err != nil || inv.Uses != 0 || inv.MaxUses != 2 {
		t.Fatalf("CreateInvite: %v %+v", err, inv)
	}
	var wg sync.WaitGroup
	var ok, invalid int32
	for i := 0; i < 8; i++ {
		u := mkUser(t, db, fmt.Sprintf("u_inv_%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.RedeemInvite(ctx, hash, u.ID)
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, room.ErrInviteInvalid):
				atomic.AddInt32(&invalid, 1)
			default:
				t.Errorf("unexpected redeem error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 2 || invalid != 6 {
		t.Fatalf("max_uses violated: ok=%d invalid=%d", ok, invalid)
	}
	// An existing member does not consume a use.
	h2 := room.HashInviteCode(uniq("code2"))
	_, _ = repo.CreateInvite(ctx, r.ID, owner.ID, h2, time.Now().Add(time.Hour), 1)
	if _, err := repo.RedeemInvite(ctx, h2, owner.ID); !errors.Is(err, room.ErrAlreadyMember) {
		t.Fatalf("already member: %v", err)
	}
	newcomer := mkUser(t, db, "u_inv_new")
	if got, err := repo.RedeemInvite(ctx, h2, newcomer.ID); err != nil || got.ID != r.ID {
		t.Fatalf("redeem after member attempt: %v", err)
	}

	// Expired and revoked invites are unusable.
	h3 := room.HashInviteCode(uniq("code3"))
	_, _ = repo.CreateInvite(ctx, r.ID, owner.ID, h3, time.Now().Add(-time.Minute), 0)
	if _, err := repo.RedeemInvite(ctx, h3, mkUser(t, db, "u_exp").ID); !errors.Is(err, room.ErrInviteInvalid) {
		t.Fatalf("expired: %v", err)
	}
	h4 := room.HashInviteCode(uniq("code4"))
	inv4, _ := repo.CreateInvite(ctx, r.ID, owner.ID, h4, time.Now().Add(time.Hour), 0)
	if err := repo.RevokeInvite(ctx, r.ID, inv4.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemInvite(ctx, h4, mkUser(t, db, "u_rev").ID); !errors.Is(err, room.ErrInviteInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	if err := repo.RevokeInvite(ctx, uuid.New(), inv4.ID); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("revoke wrong room: %v", err)
	}
	if _, err := repo.RedeemInvite(ctx, "nope", newcomer.ID); !errors.Is(err, room.ErrInviteInvalid) {
		t.Fatalf("unknown: %v", err)
	}
	invs, err := repo.ListInvites(ctx, r.ID)
	if err != nil || len(invs) != 4 {
		t.Fatalf("ListInvites: %v %d", err, len(invs))
	}
	if _, err := repo.CreateInvite(ctx, uuid.New(), owner.ID, "x", time.Now().Add(time.Hour), 0); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("invite for unknown room: %v", err)
	}
}

func TestRoomRepository_DMAndUnread(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	repo := room.NewPostgresRepository(db)
	alice := mkUser(t, db, "alice")
	bobU := mkUser(t, db, "bob")

	dm1, err := repo.OpenDM(ctx, alice.ID, bobU.Username)
	if err != nil || !dm1.IsDM || !dm1.IsPrivate {
		t.Fatalf("OpenDM: %v %+v", err, dm1)
	}
	dm2, err := repo.OpenDM(ctx, bobU.ID, alice.Username) // symmetric + idempotent
	if err != nil || dm2.ID != dm1.ID {
		t.Fatalf("OpenDM symmetry: %v %v vs %v", err, dm2.ID, dm1.ID)
	}
	if _, err := repo.OpenDM(ctx, alice.ID, alice.Username); !errors.Is(err, room.ErrSelfDM) {
		t.Fatalf("self DM: %v", err)
	}
	if _, err := repo.OpenDM(ctx, alice.ID, "no_such_user_xyz"); !errors.Is(err, room.ErrUserNotFound) {
		t.Fatalf("unknown peer: %v", err)
	}
	if _, err := repo.Update(ctx, dm1.ID, room.UpdateParams{Description: ptr("x")}); !errors.Is(err, room.ErrNotFound) {
		t.Fatalf("DMs must not be updatable: %v", err)
	}

	// Unread counters: bob's messages are unread for alice, her own are not,
	// deleted ones are skipped, and mentions are counted separately.
	insert := func(from uuid.UUID, content string, mentions []string, deleted bool) {
		var del any
		if deleted {
			del = time.Now()
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO messages (id, room_id, user_id, content, mentions, deleted_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6, now() + interval '1 second')`,
			uuid.New(), dm1.ID, from, content, mentions, del); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}
	insert(bobU.ID, "hi", []string{}, false)
	insert(bobU.ID, "hey @"+alice.Username, []string{alice.Username}, false)
	insert(bobU.ID, "oops", []string{}, true)
	insert(alice.ID, "mine", []string{}, false)

	sums, err := repo.List(ctx, alice.ID, room.ListFilter{MineOnly: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var s *room.Summary
	for i := range sums {
		if sums[i].ID == dm1.ID {
			s = &sums[i]
		}
	}
	if s == nil || s.Unread != 2 || s.UnreadMentions != 1 || s.Peer != bobU.Username || s.MemberCount != 2 || s.Role != room.RoleMember {
		t.Fatalf("unexpected summary: %+v", s)
	}
	// Reading clears the counters.
	time.Sleep(1100 * time.Millisecond) // messages are stamped now()+1s
	if err := repo.MarkRead(ctx, dm1.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	sums, _ = repo.List(ctx, alice.ID, room.ListFilter{MineOnly: true, Limit: 50})
	for _, x := range sums {
		if x.ID == dm1.ID && (x.Unread != 0 || x.UnreadMentions != 0) {
			t.Fatalf("counters not cleared: %+v", x)
		}
	}
}

func ptr[T any](v T) *T { return &v }
