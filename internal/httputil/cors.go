package httputil

import (
	"net/http"
)

// CORS returns middleware implementing an explicit cross-origin policy.
//
// Decision (conscious): the API is consumed by the terminal CLI (no Origin
// header) and by the bundled index.html demo, which is served from the same
// host as the API — same-origin requests are never subject to CORS. So the
// default is DENY: with an empty allowlist no Access-Control-Allow-Origin is
// ever emitted, and cross-origin browser calls fail. Operators that host a
// web client elsewhere add its origin(s) to the allowlist (CORS_ALLOWED_ORIGINS).
//
// Allowed origins get a reflected Allow-Origin, a credential-free policy, the
// methods/headers the API actually uses, and a handled preflight (OPTIONS).
// Disallowed origins get no CORS headers; the request still runs (the browser
// enforces the block client-side), which keeps the response caching-safe via
// Vary: Origin.
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			// No Origin header: not a CORS request (CLI, curl, same-origin JS
			// fetch). Pass through untouched.
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Reflect the request's origin (cheap, cache-hostile unless varied).
			w.Header().Add("Vary", "Origin")
			if _, ok := set[origin]; !ok {
				// Deny: emit no CORS headers. Preflight from a disallowed origin
				// falls through to the mux (which answers 405 for OPTIONS).
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				// Preflight. No credentials are used (JWT rides in a header, not a
				// cookie), so Allow-Credentials is deliberately omitted.
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
