package httputil

import (
	"net"
	"net/http"
	"strconv"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/ratelimit"
	"github.com/R-zin/vili/internal/respond"
)

// KeyFunc extracts the bucket key for a request. Returning ("", false) skips
// limiting (fail-open) when no stable key exists — e.g. an authenticated route
// reached without a user id (which cannot happen in practice, but limiting
// must never lock out traffic on an internal edge).
type KeyFunc func(r *http.Request) (string, bool)

// KeyByIP keys buckets by the client IP from RemoteAddr. Trusted proxy headers
// (X-Forwarded-For) are deliberately NOT honored: without a configured trusted
// -proxy list they are trivially spoofable, which would let an attacker rotate
// the header to dodge the limit or frame a victim IP.
func KeyByIP(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		return "", false
	}
	return "ip:" + host, true
}

// KeyByUser keys buckets by the authenticated user id from the request
// context. Requests without an authenticated user are not limited (they are
// rejected by the auth middleware anyway).
func KeyByUser(r *http.Request) (string, bool) {
	id, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		return "", false
	}
	return "user:" + id.String(), true
}

// RateLimit returns middleware that admits a request only when its key's token
// bucket allows it. On refusal it writes a 429 error envelope with a
// Retry-After hint (seconds). When lim is nil the middleware is a no-op so the
// wiring layer can disable limiting cleanly via config.
func RateLimit(lim *ratelimit.Limiter, keyFn KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if lim == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			maybeLimit(w, r, lim, keyFn, next)
		})
	}
}

// RateLimitFunc is RateLimit for an http.HandlerFunc target. Feature routes
// that must run the limiter INSIDE the auth wrapper (so the key can come from
// the authenticated context) use this form.
func RateLimitFunc(lim *ratelimit.Limiter, keyFn KeyFunc) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		if lim == nil {
			return next
		}
		return func(w http.ResponseWriter, r *http.Request) {
			maybeLimit(w, r, lim, keyFn, next)
		}
	}
}

// maybeLimit enforces the bucket for r's key, writing a 429 + Retry-After on
// refusal; otherwise it calls next.
func maybeLimit(w http.ResponseWriter, r *http.Request, lim *ratelimit.Limiter, keyFn KeyFunc, next http.Handler) {
	key, ok := keyFn(r)
	if ok && !lim.Allow(key) {
		retry := lim.RetryAfter(key)
		// Round up: a client waiting exactly retry seconds might still be early
		// on a sub-second boundary.
		secs := int(retry.Seconds() + 0.5)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		respond.Error(w, http.StatusTooManyRequests, "rate_limited", "too many requests, slow down")
		return
	}
	next.ServeHTTP(w, r)
}
