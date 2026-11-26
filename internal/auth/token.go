package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the registered JWT claims plus the subject user id (standard
// "sub" claim), the login session the token belongs to ("sid", the
// refresh-token family) and the user's token version at mint time ("ver").
// Revoking the session or bumping the version invalidates the token at once.
type Claims struct {
	jwt.RegisteredClaims
	SessionID string `json:"sid,omitempty"`
	Version   int    `json:"ver,omitempty"`
}

// Identity is what a verified access token says about its bearer.
type Identity struct {
	UserID uuid.UUID
	// Session is the refresh-token family the token was minted for; uuid.Nil
	// for tokens minted without one.
	Session uuid.UUID
	// Version is the user's token version when the token was minted.
	Version int
}

// TokenService creates and verifies HS256 JWTs. The secret is injected at
// construction; an empty secret is refused so tokens can never be forged via
// a configuration fallback.
type TokenService struct {
	secret []byte
	expiry time.Duration
	// validator, when set, is consulted on every authenticated request so
	// revoked sessions are rejected before their tokens expire.
	validator Validator
}

// Validator decides whether a cryptographically valid token's session is
// still live (not logged out, not revoked, user still exists).
type Validator interface {
	Valid(ctx context.Context, id Identity) (bool, error)
}

// SetValidator installs v; call it during wiring, before serving.
func (s *TokenService) SetValidator(v Validator) { s.validator = v }

// MinJWTSecretBytes is the minimum key length enforced for HS256 signing (RFC 7518 § 3.2).
const MinJWTSecretBytes = 32

// NewTokenService builds a TokenService. It returns an error if the secret is
// shorter than MinJWTSecretBytes (32 bytes / 256 bits) or the expiry is non-positive.
func NewTokenService(secret []byte, expiry time.Duration) (*TokenService, error) {
	if len(secret) == 0 {
		return nil, errors.New("auth: JWT secret must not be empty")
	}
	if len(secret) < MinJWTSecretBytes {
		return nil, fmt.Errorf("auth: JWT secret must be at least %d bytes (256 bits), got %d", MinJWTSecretBytes, len(secret))
	}
	if expiry <= 0 {
		return nil, fmt.Errorf("auth: JWT expiry must be positive, got %s", expiry)
	}
	// Copy so callers cannot mutate the secret after construction.
	s := make([]byte, len(secret))
	copy(s, secret)
	return &TokenService{secret: s, expiry: expiry}, nil
}

// Expiry returns the configured access-token lifetime.
func (s *TokenService) Expiry() time.Duration { return s.expiry }

// Issue mints a signed token whose subject is the given user id, not bound to
// a session, at token version 0.
func (s *TokenService) Issue(userID uuid.UUID) (string, error) {
	return s.IssueFor(Identity{UserID: userID})
}

// IssueFor mints a signed token for id (subject, session and version).
func (s *TokenService) IssueFor(id Identity) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   id.UserID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.expiry)),
		},
		Version: id.Version,
	}
	if id.Session != uuid.Nil {
		claims.SessionID = id.Session.String()
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates a token, returning the subject user id. It
// enforces the HS256 signing method (rejecting alg-confusion), requires an
// expiry, and extracts the user id without any panicking type assertion.
// Malformed, expired, wrong-alg, or otherwise invalid tokens yield an error.
func (s *TokenService) Verify(tokenString string) (uuid.UUID, error) {
	id, err := s.VerifyIdentity(tokenString)
	return id.UserID, err
}

// VerifyIdentity is Verify returning the token's full Identity.
func (s *TokenService) VerifyIdentity(tokenString string) (Identity, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(
		tokenString,
		claims,
		func(t *jwt.Token) (any, error) {
			// Enforce HMAC so an attacker cannot switch algorithms.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method %q", t.Method.Alg())
			}
			return s.secret, nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verify token: %w", err)
	}
	if !token.Valid {
		return Identity{}, errors.New("auth: invalid token")
	}

	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		return Identity{}, errors.New("auth: token has no subject")
	}
	userID, err := uuid.Parse(subject)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: token subject is not a uuid: %w", err)
	}
	id := Identity{UserID: userID, Version: claims.Version}
	if claims.SessionID != "" {
		if id.Session, err = uuid.Parse(claims.SessionID); err != nil {
			return Identity{}, fmt.Errorf("auth: token session is not a uuid: %w", err)
		}
	}
	return id, nil
}
