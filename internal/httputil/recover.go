package httputil

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/R-zin/vili/internal/respond"
)

// Recover returns middleware that converts a panic in a downstream handler
// into a uniform 500 error envelope instead of dropping the connection. The
// panic value, route, and stack are logged server-side; the client sees only
// the generic envelope (no internals leak). It re-panics on http.ErrAbortHandler
// so net/http's own semantics for aborted upgrades are preserved.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is net/http's control-flow signal (e.g.
				// during hijack); it must propagate, not become a 500.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.Error("panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				// The handler may have already written a status; respond.Error is
				// a best effort and the encoder logs if the write fails.
				respond.Error(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
