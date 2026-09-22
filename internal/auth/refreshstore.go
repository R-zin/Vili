package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrRefreshNotFound is returned when no refresh token matches a presented
// digest. Mapped to 401 by the caller.
var ErrRefreshNotFound = errors.New("auth: refresh token not found")

// RefreshToken is one stored refresh-token row (the digest itself is never
// exposed). Consumed marks a token that has already been rotated; presenting a
// consumed token indicates theft and triggers family revocation.
type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	FamilyID  uuid.UUID
	ExpiresAt time.Time
	Consumed  bool
}

// RefreshStore persists and rotates refresh tokens. All lookups are by digest
// (never the raw token). It is satisfied by the user feature's Postgres
// repository at the wiring layer; defining the interface here keeps token
// semantics out of the user feature.
type RefreshStore interface {
	// Insert stores a new token digest in familyID, returning its id.
	InsertRefreshToken(ctx context.Context, userID, familyID uuid.UUID, hash string, expiresAt time.Time) (uuid.UUID, error)
	// RefreshTokenByHash returns the token row for a digest, or ErrRefreshNotFound.
	RefreshTokenByHash(ctx context.Context, hash string) (RefreshToken, error)
	// ConsumeRefreshToken marks a token used and links it to its replacement
	// (rotation).
	ConsumeRefreshToken(ctx context.Context, id, replacedBy uuid.UUID) error
	// RevokeRefreshFamily deletes every token in a family (reuse detected).
	RevokeRefreshFamily(ctx context.Context, familyID uuid.UUID) error
	// RevokeRefreshToken deletes a single token by digest (logout).
	RevokeRefreshToken(ctx context.Context, hash string) error
}
