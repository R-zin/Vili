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
		// Browsers must not sniff content off declared MIME types.
		h.Set("X-Content-Type-Options", "nosniff")
		// Block clickjacking across the entire API and demo.
		h.Set("X-Frame-Options", "DENY")
		// Prevent referrer leaks to cross-origin destinations.
		h.Set("Referrer-Policy", "no-referrer")
		// HTTP Strict Transport Security (HSTS): enforce HTTPS for 2 years.
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains; preload")
		// Disable browser device features (camera, mic, etc.)
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		// Modern cross-origin isolation headers against Spectre attacks.
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		// Prevent caching of sensitive JSON API responses by intermediate proxies.
		h.Set("Cache-Control", "no-store, no-cache, must-revalidate")
		h.Set("Pragma", "no-cache")
		// Content Security Policy
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
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
