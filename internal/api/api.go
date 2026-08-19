// Package api is the thin wiring layer. It composes each feature package's
// routes onto a single http.ServeMux, mounts the shared auth middleware around
// the protected routes, and adds the health/readiness endpoints. It holds no
// business logic; features implement their own handlers.
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

// NewRouter builds the application's HTTP handler. tokens provides the auth
// middleware for protected routes; db backs the readiness probe. realtime may
// be nil, in which case no websocket route is registered (REST-only).
func NewRouter(
	users *user.Handler,
	rooms *room.Handler,
	messages *message.Handler,
	realtime *ws.Handler,
	tokens *auth.TokenService,
	db Pinger,
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

	// Public auth routes.
	users.RegisterRoutes(mux)

	// Protected feature routes, mounted behind the shared auth middleware.
	require := tokens.Require
	rooms.RegisterRoutes(mux, require)
	messages.RegisterRoutes(mux, require)
	if realtime != nil {
		realtime.RegisterRoutes(mux, tokens.RequireWS)
	}

	return mux
}
