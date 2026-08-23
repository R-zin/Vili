package httputil

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/R-zin/vili/internal/metrics"
)

// statusWriter wraps http.ResponseWriter to capture the status code and byte
// count, defaulting to 200 when the handler writes a body without an explicit
// WriteHeader. It preserves the optional interfaces the runtime and the
// websocket upgrade rely on (Flusher, Hijacker via http.ResponseController).
type statusWriter struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

// Flush forwards streaming flushes to the underlying writer.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController can
// reach the Hijacker for the websocket upgrade through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Observe returns middleware that times each request on the way through,
// records it in reg (when non-nil), and emits one structured access log line
// per request (when log is true). It never logs the Authorization header,
// query strings (which may carry a ?token= websocket credential), or any
// request/response body — only method, route pattern, status, and duration.
func Observe(reg *metrics.Registry, log bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			var release func()
			if reg != nil {
				release = reg.IncInFlight()
			}

			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			dur := time.Since(start)

			if release != nil {
				release()
			}
			// r.Pattern is the matched "<METHOD> <pattern>" (Go 1.22); strip the
			// method prefix for the route label. Empty for 404/405 fall-through.
			route := routePattern(r)

			if reg != nil {
				reg.Observe(r.Method, route, status, dur)
			}
			if log {
				slog.Info("request",
					"method", r.Method,
					"route", route,
					"status", status,
					"duration_ms", dur.Milliseconds(),
				)
			}
		})
	}
}

// routePattern extracts the route template from the request's matched pattern,
// dropping the leading method token. An unmatched request (404/405) yields the
// literal path so it is still visible but low-cardinality enough for logs
// (metrics use matched routes almost exclusively).
func routePattern(r *http.Request) string {
	p := r.Pattern
	for i := 0; i < len(p); i++ {
		if p[i] == ' ' {
			return p[i+1:]
		}
	}
	if p != "" {
		return p
	}
	return "unmatched"
}
