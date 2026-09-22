package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// refreshTokenBytes is the entropy of a generated refresh token. 256 bits is
// far beyond brute-force, so a fast hash (SHA-256) — not a password KDF — is
// the correct storage transform.
const refreshTokenBytes = 32

// GenerateRefresh returns a new opaque refresh token and the hex SHA-256
// digest that is stored. The token itself is returned to the client once and
// never persisted; only the digest is kept, so a database leak yields nothing
// usable.
func GenerateRefresh() (token string, hash string, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashRefresh(token), nil
}

// HashRefresh returns the hex SHA-256 digest of a presented refresh token, for
// lookup against the stored digest.
func HashRefresh(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CompareDigests reports constant-time equality of two hex digests, so token
// lookup does not leak a timing signal.
func CompareDigests(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
