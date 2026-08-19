package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/R-zin/vili/internal/respond"
)

// contextKey is an unexported type for context keys defined in this package,
// preventing collisions with keys set by other packages.
type contextKey int

// userIDKey keys the authenticated user id stored in the request context.
const userIDKey contextKey = iota

// Middleware returns HTTP middleware that requires a valid
// "Authorization: Bearer <token>" header. It verifies the token, parses the
// subject to a uuid, and stores it in the request context. Missing or invalid
// tokens yield a 401 error envelope and the handler is not called.
func (s *TokenService) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenString, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "missing or malformed authorization header")
			return
		}
		userID, err := s.Verify(tokenString)
		if err != nil {
			respond.Error(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Require wraps an http.HandlerFunc with the auth middleware.
func (s *TokenService) Require(next http.HandlerFunc) http.Handler {
	return s.Middleware(next)
}

// RequireWS is Require plus a fallback to a "token" query parameter. Browser
// WebSocket clients cannot set an Authorization header on the handshake, so
// the realtime route authenticates via the header (the CLI) or, when the
// header is absent, the query token (e.g. the bundled index.html demo). REST
// routes keep using Require (header only): a query token can leak into
// logs/caches, so it is accepted only on this realtime route.
func (s *TokenService) RequireWS(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if q := r.URL.Query().Get("token"); q != "" {
				r.Header.Set("Authorization", "Bearer "+q)
			}
		}
		s.Middleware(next).ServeHTTP(w, r)
	})
}

// UserIDFromContext returns the authenticated user id stored by Middleware.
// The second return value reports whether a user id was present.
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userIDKey).(uuid.UUID)
	return id, ok
}

// bearerToken extracts the token from an "Authorization: Bearer <tok>" header.
// It requires exactly the Bearer scheme followed by a single space and a
// non-empty token; anything else is rejected.
func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}
