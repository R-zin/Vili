package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeLoader serves SessionState from a map and counts loads.
type fakeLoader struct {
	mu     sync.Mutex
	states map[uuid.UUID]SessionState // keyed by user; SessionLive applies to any session
	dead   map[uuid.UUID]bool         // sessions that no longer exist
	loads  int
	err    error
	// during, if set, runs inside a load (to simulate a concurrent revocation).
	during func()
}

func (f *fakeLoader) SessionState(ctx context.Context, user, session uuid.UUID) (SessionState, error) {
	f.mu.Lock()
	f.loads++
	during := f.during
	f.during = nil
	st, ok := f.states[user]
	dead := f.dead[session]
	err := f.err
	f.mu.Unlock()
	if during != nil {
		during()
	}
	if err != nil {
		return SessionState{}, err
	}
	if !ok {
		return SessionState{}, nil
	}
	st.SessionLive = !dead
	return st, nil
}

func newLoader(user uuid.UUID, version int) *fakeLoader {
	return &fakeLoader{states: map[uuid.UUID]SessionState{user: {UserExists: true, Version: version}}, dead: map[uuid.UUID]bool{}}
}

func TestSessionCache_ValidityRules(t *testing.T) {
	user, sess := uuid.New(), uuid.New()
	l := newLoader(user, 2)
	c := NewSessionCache(l, time.Minute)
	ctx := context.Background()

	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"current version + live session", Identity{UserID: user, Session: sess, Version: 2}, true},
		{"old version", Identity{UserID: user, Session: sess, Version: 1}, false},
		{"no session claim (legacy token) at current version", Identity{UserID: user, Version: 2}, true},
		{"unknown user", Identity{UserID: uuid.New(), Version: 0}, false},
	}
	for _, tc := range cases {
		if got, err := c.Valid(ctx, tc.id); err != nil || got != tc.want {
			t.Errorf("%s: got %v %v want %v", tc.name, got, err, tc.want)
		}
	}
	l.dead[sess] = true
	c.Invalidate(user)
	if ok, _ := c.Valid(ctx, Identity{UserID: user, Session: sess, Version: 2}); ok {
		t.Fatal("dead session must be rejected after invalidation")
	}
}

func TestSessionCache_CachesUntilInvalidatedOrExpired(t *testing.T) {
	user := uuid.New()
	l := newLoader(user, 0)
	c := NewSessionCache(l, time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	id := Identity{UserID: user}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = c.Valid(ctx, id)
	}
	if l.loads != 1 {
		t.Fatalf("expected 1 load, got %d", l.loads)
	}
	// Revocation: version bumped in the store and the cache invalidated.
	l.states[user] = SessionState{UserExists: true, Version: 1}
	now = now.Add(time.Millisecond)
	c.Invalidate(user)
	now = now.Add(time.Millisecond)
	if ok, _ := c.Valid(ctx, id); ok {
		t.Fatal("token with the old version must be rejected right after invalidation")
	}
	if l.loads != 2 {
		t.Fatalf("invalidation must force a reload, loads=%d", l.loads)
	}
	// TTL expiry is the fallback when an invalidation is missed.
	l.states[user] = SessionState{UserExists: true, Version: 0}
	now = now.Add(2 * time.Minute)
	if ok, _ := c.Valid(ctx, id); !ok || l.loads != 3 {
		t.Fatalf("expired entry must reload: ok=%v loads=%d", ok, l.loads)
	}
}

// A load that began before a revocation must not be cached, or the
// pre-revocation answer could outlive the revocation for a whole TTL.
func TestSessionCache_RaceWithRevocationIsNotCached(t *testing.T) {
	user := uuid.New()
	l := newLoader(user, 0)
	c := NewSessionCache(l, time.Minute)
	tick := time.Now()
	c.now = func() time.Time { tick = tick.Add(time.Millisecond); return tick }
	l.during = func() { c.Invalidate(user) } // revocation lands mid-load
	id := Identity{UserID: user}
	if ok, _ := c.Valid(context.Background(), id); !ok {
		t.Fatal("the in-flight answer itself is still returned")
	}
	_, _ = c.Valid(context.Background(), id)
	if l.loads != 2 {
		t.Fatalf("a load racing a revocation must not be cached (loads=%d)", l.loads)
	}
}

func TestSessionCache_ErrorsAndBound(t *testing.T) {
	user := uuid.New()
	l := newLoader(user, 0)
	l.err = errors.New("db down")
	c := NewSessionCache(l, time.Minute)
	if _, err := c.Valid(context.Background(), Identity{UserID: user}); err == nil {
		t.Fatal("loader errors must surface (fail closed)")
	}
	l.err = nil
	// Fill past the bound with distinct sessions; the cache must stay bounded.
	for i := 0; i < sessionCacheMax+10; i++ {
		_, _ = c.Valid(context.Background(), Identity{UserID: user, Session: uuid.New()})
	}
	if c.size > sessionCacheMax {
		t.Fatalf("cache exceeded its bound: %d", c.size)
	}
}

func TestIssueForAndVerifyIdentity(t *testing.T) {
	ts, _ := NewTokenService([]byte("identity-test-secret-at-least-32-bytes!"), time.Hour)
	want := Identity{UserID: uuid.New(), Session: uuid.New(), Version: 7}
	tok, err := ts.IssueFor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ts.VerifyIdentity(tok)
	if err != nil || got != want {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	legacy, _ := ts.Issue(want.UserID)
	if got, _ := ts.VerifyIdentity(legacy); got.Session != uuid.Nil || got.Version != 0 {
		t.Fatalf("legacy token identity: %+v", got)
	}
}

type stubValidator struct {
	ok  bool
	err error
	got Identity
}

func (s *stubValidator) Valid(ctx context.Context, id Identity) (bool, error) {
	s.got = id
	return s.ok, s.err
}

func TestMiddlewareConsultsValidator(t *testing.T) {
	ts, _ := NewTokenService([]byte("middleware-validator-secret-32-bytes!!"), time.Hour)
	id := Identity{UserID: uuid.New(), Session: uuid.New(), Version: 3}
	tok, _ := ts.IssueFor(id)
	var gotSession uuid.UUID
	h := ts.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession, _ = SessionIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if do() != http.StatusNoContent {
		t.Fatal("no validator: token alone suffices")
	}
	v := &stubValidator{ok: true}
	ts.SetValidator(v)
	if do() != http.StatusNoContent || v.got != id || gotSession != id.Session {
		t.Fatalf("validator must see the identity and the session must reach the context: %+v %v", v.got, gotSession)
	}
	v.ok = false
	if code := do(); code != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", code)
	}
	v.err = errors.New("db down")
	if code := do(); code != http.StatusServiceUnavailable {
		t.Fatalf("validator failure must fail closed with 503, got %d", code)
	}
}
