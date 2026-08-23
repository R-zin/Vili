// Package api is the thin wiring layer. It composes each feature package's
// routes onto a single http.ServeMux, mounts the shared auth middleware around
// the protected routes, adds the health/readiness endpoints, and wraps the
// whole router in the Phase-5 cross-cutting middleware (security headers, body
// limits, recovery, CORS, logging/metrics) plus the targeted rate limiters.
// It holds no business logic; features implement their own handlers.
package api

import (
	"context"
	"net/http"

	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/message"
	"github.com/R-zin/vili/internal/respond"
	"github.com/R-zin/vili/internal/room"
	"github.com/R-zin/vili/internal/user"
	"github.com/R-zin/vili/internal/ws"
)

// Pinger reports database liveness for the readiness endpoint.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Middleware carries the cross-cutting wrappers built once in main and applied
// here. Each field may be nil, in which case the corresponding concern is
// skipped, keeping the no-config Phase-4 wiring (used by tests) working
// unchanged.
type Middleware struct {
	// Chain wraps the entire router (recovery, headers, CORS, body cap,
	// logging/metrics). Order matters; build it with httputil.Chain.
	Chain func(http.Handler) http.Handler
	// AuthLimit rate-limits the public /v1/auth/* routes by IP.
	AuthLimit func(http.Handler) http.Handler
	// MessageLimit rate-limits the protected message POST by user. It runs
	// inside the auth wrapper so it keys on the authenticated user id.
	MessageLimit func(http.HandlerFunc) http.HandlerFunc
	// Metrics, when non-nil, mounts GET /metrics in Prometheus text format.
	Metrics http.Handler
	// Pprof, when non-nil, mounts the net/http/pprof handlers under
	// /debug/pprof (only ever set behind an env flag defaulting to off).
	Pprof http.Handler
}

// NewRouter builds the application's HTTP handler. tokens provides the auth
// middleware for protected routes; db backs the readiness probe. realtime may
// be nil, in which case no websocket route is registered (REST-only). mw is
// the optional middleware bundle; its zero value composes the bare Phase-4
// router.
func NewRouter(
	users *user.Handler,
	rooms *room.Handler,
	messages *message.Handler,
	realtime *ws.Handler,
	tokens *auth.TokenService,
	db Pinger,
	mw Middleware,
) http.Handler {
	mux := http.NewServeMux()

	// Public liveness: no database access.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		respond.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Readiness: 200 when the database answers a ping, else 503.
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			respond.Error(w, http.StatusServiceUnavailable, "unavailable", "database is not reachable")
			return
		}
		respond.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// Observability endpoints. Not part of the versioned API contract; the
	// metrics handler renders Prometheus text, pprof is debug-only.
	if mw.Metrics != nil {
		mux.Handle("GET /metrics", mw.Metrics)
	}
	if mw.Pprof != nil {
		mux.Handle("/debug/pprof/", mw.Pprof)
	}

	// Public auth routes, wrapped in the per-IP limiter when configured.
	users.RegisterRoutes(mux, mw.AuthLimit)

	// Protected feature routes, mounted behind the shared auth middleware.
	require := tokens.Require
	rooms.RegisterRoutes(mux, require)
	messages.RegisterRoutes(mux, require, mw.MessageLimit)
	if realtime != nil {
		realtime.RegisterRoutes(mux, tokens.RequireWS)
	}

	var handler http.Handler = mux
	if mw.Chain != nil {
		handler = mw.Chain(handler)
	}
	return handler
}
