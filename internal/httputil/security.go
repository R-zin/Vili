// Package httputil holds the cross-cutting HTTP middleware the API composes
// around its feature routes: security headers, request-body size limits,
// panic recovery, an explicit CORS policy, request logging, metrics
// instrumentation, and rate limiting. Each piece is small, self-contained,
// and individually unit-tested with httptest; the wiring layer
// (internal/api) orders and mounts them.
package httputil

import (
	"net/http"
)

// SecurityHeaders returns middleware that sets defensive response headers on
// every response. The API serves JSON only (plus the bundled index.html demo
// and the /metrics text endpoint), so the CSP is restrictive and
// self-contained: no remote origins, no framing by others.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Browsers must not sniff content off the declared application/json (or
		// the demo's text/html) content type.
		h.Set("X-Content-Type-Options", "nosniff")
		// The API has no reason to be framed; blocks clickjacking of the demo.
		h.Set("X-Frame-Options", "DENY")
		// Send no referrer to cross-origin requests.
		h.Set("Referrer-Policy", "no-referrer")
		// CSP: the JSON API has no scripts/styles; the self-contained demo uses
		// only inline script/style, so allow those from 'self' only. This is a
		// defense-in-depth value for the one HTML page the server might serve.
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self' ws: wss:; img-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// MaxBodyBytes returns middleware that bounds the size of every request body
// using http.MaxBytesReader. Handlers that decode JSON then surface a
// "request entity too large" style failure instead of reading unbounded
// input. GET/HEAD requests typically carry no body; the cap is harmless there.
func MaxBodyBytes(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
