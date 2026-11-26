// Package user is the user feature: it owns the User type, its repository
// (against *sql.DB via pgx stdlib), and the HTTP handlers for register/login.
// It is self-contained; other features never import it.
package user

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// User is an account. PasswordHash is never serialized to clients.
type User struct {
	ID           uuid.UUID `json:"-"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	// TokenVersion is embedded in access tokens; bumping it revokes them all.
	TokenVersion int `json:"-"`
}

// Public is the safe, client-facing representation: it never includes the
// password hash.
type Public struct {
	ID        uuid.UUID `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

// Public returns the safe view of u.
func (u User) Public() Public {
	return Public{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt}
}

// Sentinel errors mapped to HTTP responses by the handler.
var (
	ErrNotFound      = errors.New("user not found")
	ErrUsernameTaken = errors.New("username already taken")
)

// Repository is the narrow persistence interface the Handler depends on. It
// is satisfied by PostgresRepository in production and by in-memory fakes in
// tests.
type Repository interface {
	// Create inserts a new user, returning ErrUsernameTaken on conflict.
	Create(ctx context.Context, username, passwordHash string) (User, error)
	// ByUsername looks up a user, returning ErrNotFound when absent.
	ByUsername(ctx context.Context, username string) (User, error)
	// ByID looks up a user by id, returning ErrNotFound when absent. It backs
	// token refresh, which authenticates by the id stored on the refresh token.
	ByID(ctx context.Context, id uuid.UUID) (User, error)
	// UsernameByID returns only the username for an id, returning ErrNotFound
	// when absent. It backs realtime presence, which authenticates by id.
	UsernameByID(ctx context.Context, id uuid.UUID) (string, error)
	// UpdatePassword replaces the stored password hash (ErrNotFound if absent).
	UpdatePassword(ctx context.Context, id uuid.UUID, passwordHash string) error
	// Delete removes the account and, via cascades, everything it owns.
	Delete(ctx context.Context, id uuid.UUID) error
	// BumpTokenVersion increments the user's token version, invalidating every
	// access token minted before, and returns the new version.
	BumpTokenVersion(ctx context.Context, id uuid.UUID) (int, error)
}

// Revoker is told when sessions end so every instance can drop cached session
// state and close the affected live connections at once. session is uuid.Nil
// for "all of the user's sessions". It is satisfied by the realtime hub at the
// wiring layer; nil disables it.
type Revoker interface {
	RevokeSession(userID, session uuid.UUID)
}
