package auth

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionState is the server-side truth about a token's user and session.
type SessionState struct {
	// UserExists is false once the account is deleted.
	UserExists bool
	// Version is the user's current token version.
	Version int
	// SessionLive reports whether the token's session (refresh-token family)
	// still exists; always true for tokens without a session.
	SessionLive bool
}

// SessionLoader reads SessionState from the database.
type SessionLoader interface {
	SessionState(ctx context.Context, userID, session uuid.UUID) (SessionState, error)
}

// sessionCacheMax bounds the cache; past it, expired entries are swept and, if
// still full, the cache is reset (it only ever costs extra lookups).
const sessionCacheMax = 50_000

// SessionCache validates tokens against the session store with a short-lived
// cache, so most requests cost no database round trip. Revocations call
// Invalidate (on every instance, via the cluster bus), which takes effect
// immediately; the TTL bounds staleness should an invalidation be missed.
type SessionCache struct {
	load SessionLoader
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[uuid.UUID]map[uuid.UUID]cacheEntry // user -> session -> state
	revoked map[uuid.UUID]time.Time                // user -> last invalidation
	size    int
}

type cacheEntry struct {
	state   SessionState
	expires time.Time
}

// NewSessionCache builds a cache over load with the given TTL.
func NewSessionCache(load SessionLoader, ttl time.Duration) *SessionCache {
	return &SessionCache{
		load:    load,
		ttl:     ttl,
		now:     time.Now,
		entries: map[uuid.UUID]map[uuid.UUID]cacheEntry{},
		revoked: map[uuid.UUID]time.Time{},
	}
}

// Valid reports whether id's user exists, its version is current and its
// session is live.
func (c *SessionCache) Valid(ctx context.Context, id Identity) (bool, error) {
	st, err := c.state(ctx, id.UserID, id.Session)
	if err != nil {
		return false, err
	}
	return st.UserExists && st.Version == id.Version && st.SessionLive, nil
}

func (c *SessionCache) state(ctx context.Context, user, session uuid.UUID) (SessionState, error) {
	start := c.now()
	c.mu.Lock()
	if e, ok := c.entries[user][session]; ok && start.Before(e.expires) {
		c.mu.Unlock()
		return e.state, nil
	}
	c.mu.Unlock()

	st, err := c.load.SessionState(ctx, user, session)
	if err != nil {
		return SessionState{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// A revocation that landed while we were loading may have been read too
	// late by our query; don't cache a possibly pre-revocation answer.
	if inv, ok := c.revoked[user]; ok && !start.After(inv) {
		return st, nil
	}
	if c.size >= sessionCacheMax {
		c.sweep()
	}
	m := c.entries[user]
	if m == nil {
		m = map[uuid.UUID]cacheEntry{}
		c.entries[user] = m
	}
	if _, exists := m[session]; !exists {
		c.size++
	}
	m[session] = cacheEntry{state: st, expires: c.now().Add(c.ttl)}
	return st, nil
}

// Invalidate drops every cached entry for user; the next request reloads.
func (c *SessionCache) Invalidate(user uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.size -= len(c.entries[user])
	delete(c.entries, user)
	c.revoked[user] = c.now()
}

// sweep removes expired entries and stale revocation marks; if the cache is
// still full it starts over. Called with mu held.
func (c *SessionCache) sweep() {
	now := c.now()
	for user, m := range c.entries {
		for s, e := range m {
			if !now.Before(e.expires) {
				delete(m, s)
				c.size--
			}
		}
		if len(m) == 0 {
			delete(c.entries, user)
		}
	}
	for user, at := range c.revoked {
		if now.Sub(at) > c.ttl {
			delete(c.revoked, user)
		}
	}
	if c.size >= sessionCacheMax {
		c.entries = map[uuid.UUID]map[uuid.UUID]cacheEntry{}
		c.size = 0
	}
}
