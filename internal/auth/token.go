package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims are the registered JWT claims plus the subject user id. The user id
// is carried in the standard "sub" claim.
type Claims struct {
	jwt.RegisteredClaims
}

// TokenService creates and verifies HS256 JWTs. The secret is injected at
// construction; an empty secret is refused so tokens can never be forged via
// a configuration fallback.
type TokenService struct {
	secret []byte
	expiry time.Duration
}

// NewTokenService builds a TokenService. It returns an error if the secret is
// empty or the expiry is non-positive.
func NewTokenService(secret []byte, expiry time.Duration) (*TokenService, error) {
	if len(secret) == 0 {
		return nil, errors.New("auth: JWT secret must not be empty")
	}
	if expiry <= 0 {
		return nil, fmt.Errorf("auth: JWT expiry must be positive, got %s", expiry)
	}
	// Copy so callers cannot mutate the secret after construction.
	s := make([]byte, len(secret))
	copy(s, secret)
	return &TokenService{secret: s, expiry: expiry}, nil
}

// Issue mints a signed token whose subject is the given user id.
func (s *TokenService) Issue(userID uuid.UUID) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.expiry)),
		},
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
		return uuid.Nil, fmt.Errorf("auth: verify token: %w", err)
	}
	if !token.Valid {
		return uuid.Nil, errors.New("auth: invalid token")
	}

	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		return uuid.Nil, errors.New("auth: token has no subject")
	}
	userID, err := uuid.Parse(subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: token subject is not a uuid: %w", err)
	}
	return userID, nil
}
