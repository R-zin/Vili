// Command server is the Vili backend's only HTTP entrypoint. It loads config,
// connects to Postgres, runs migrations, wires the feature packages onto one
// router, and serves until interrupted.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/R-zin/vili/internal/api"
	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/config"
	"github.com/R-zin/vili/internal/httputil"
	"github.com/R-zin/vili/internal/message"
	"github.com/R-zin/vili/internal/metrics"
	"github.com/R-zin/vili/internal/ratelimit"
	"github.com/R-zin/vili/internal/room"
	"github.com/R-zin/vili/internal/server"
	"github.com/R-zin/vili/internal/store"
	"github.com/R-zin/vili/internal/user"
	"github.com/R-zin/vili/internal/ws"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := store.Open(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("closing database", "error", err)
		}
	}()

	applied, err := store.Migrate(context.Background(), db)
	if err != nil {
		return err
	}
	slog.Info("migrations applied", "count", len(applied), "versions", applied)

	tokens, err := auth.NewTokenService(cfg.JWTSecret, cfg.JWTExpiry)
	if err != nil {
		return err
	}

	// One message repository is shared: it gates REST history/posts and,
	// through the realtime hub, membership for the websocket route. The hub
	// fans freshly-posted messages out to live connections.
	hub := ws.NewHub()
	hub.SetMaxConnsPerUser(cfg.WS.MaxConnsPerUser)
	messageRepo := message.NewPostgresRepository(db)
	userRepo := user.NewPostgresRepository(db)

	// Wire each feature package's handler onto a single router, behind the
	// Phase-5 middleware bundle (hardening, observability, rate limiting).
	mw, stop := buildMiddleware(cfg)
	defer stop()
	handler := api.NewRouter(
		user.NewHandler(userRepo, tokens, userRepo, cfg.RefreshTokenTTL),
		room.NewHandler(room.NewPostgresRepository(db)),
		message.NewHandler(messageRepo, hub),
		ws.NewHandler(hub, messageRepo, userRepo),
		tokens,
		db,
		mw,
	)

	// Close live websocket connections when the server stops so they don't
	// outlive the HTTP drain.
	defer hub.Close()

	return server.Run(context.Background(), cfg, handler)
}

// buildMiddleware assembles the Phase-5 cross-cutting middleware from config.
// It returns the bundle passed to api.NewRouter and a cleanup func that stops
// the rate limiters' background sweepers. Everything is gated by config so a
// zero-value-friendly composition (tests) still works: each knob can be turned
// off without recompiling.
func buildMiddleware(cfg config.Config) (api.Middleware, func()) {
	var mw api.Middleware
	var closers []func()

	// Observability: one registry feeds both the request instrumentation and
	// the /metrics endpoint.
	var reg *metrics.Registry
	if cfg.Observability.MetricsEnabled {
		reg = metrics.New()
		mw.Metrics = reg.Handler()
	}

	// Rate limiting: per-IP on auth, per-user on message POSTs.
	if cfg.RateLimit.Enabled {
		authLim := ratelimit.New(cfg.RateLimit.AuthRPS, cfg.RateLimit.AuthBurst)
		msgLim := ratelimit.New(cfg.RateLimit.MessagesRPS, cfg.RateLimit.MessagesBurst)
		closers = append(closers, authLim.Stop, msgLim.Stop)
		mw.AuthLimit = httputil.RateLimit(authLim, httputil.KeyByIP)
		mw.MessageLimit = httputil.RateLimitFunc(msgLim, httputil.KeyByUser)
	}

	// pprof: mounted only behind the explicit, default-off flag.
	if cfg.Observability.PprofEnabled {
		mw.Pprof = httputil.PprofHandler()
		slog.Warn("pprof endpoints enabled at /debug/pprof — do not expose this in production")
	}

	// Router-wide chain, outermost first: recovery so any panic still yields a
	// 500 envelope; observation so every request is timed/counted/logged;
	// security headers and CORS; the body cap closest to the handlers.
	var headersMw func(http.Handler) http.Handler
	if cfg.Security.HeadersEnabled {
		headersMw = httputil.SecurityHeaders
	}
	mw.Chain = func(next http.Handler) http.Handler {
		return httputil.Chain(next,
			httputil.Recover,
			httputil.Observe(reg, cfg.Observability.RequestLogEnabled),
			headersMw,
			httputil.CORS(cfg.Security.CORSAllowedOrigins),
			httputil.MaxBodyBytes(cfg.Security.MaxBodyBytes),
		)
	}

	return mw, func() {
		for _, c := range closers {
			c()
		}
	}
}
