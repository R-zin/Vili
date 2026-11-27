// Command server is the Vili backend's only HTTP entrypoint. It loads config,
// connects to Postgres, runs migrations, wires the feature packages onto one
// router, and serves until interrupted.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/R-zin/vili/internal/api"
	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/bus"
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

	// Background cleanup of expired refresh tokens and invites; stops when run
	// returns.
	janitorCtx, stopJanitor := context.WithCancel(context.Background())
	defer stopJanitor()
	go store.RunJanitor(janitorCtx, db, time.Hour)

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

	// Cluster wiring: the bus carries broadcasts, evictions and revocations to
	// every instance; presence is shared through Postgres.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	instance := cfg.WS.InstanceID
	if instance == "" {
		host, _ := os.Hostname()
		instance = bus.NewInstanceID(host)
	}
	var (
		eventBus bus.Bus = bus.NewLocal()
		pgBus    *bus.Postgres
	)
	if cfg.WS.Bus == "postgres" {
		pgBus = bus.NewPostgres(db, cfg.DatabaseURL, instance)
		eventBus = pgBus
		ready := make(chan struct{})
		go pgBus.Run(bgCtx, ready)
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			slog.Warn("realtime bus not connected yet; /ready reports 503 until it is")
		}
		presence := ws.NewPGPresence(db, instance, hub.LocalPresence)
		if err := presence.Register(context.Background()); err != nil {
			return err
		}
		hub.UsePresence(presence)
		go presence.Run(bgCtx, ws.PresenceHeartbeat)
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := presence.Deregister(ctx); err != nil {
				slog.Warn("presence deregister failed", "error", err)
			}
		}()
	}
	hub.UseBus(eventBus)
	slog.Info("realtime", "bus", cfg.WS.Bus, "instance", instance)

	// Access tokens are checked against the session store (cached), so logout,
	// "sign out everywhere", password changes and account deletion take effect
	// at once on every instance; the bus invalidates every instance's cache.
	sessions := auth.NewSessionCache(userRepo, cfg.WS.SessionCacheTTL)
	tokens.SetValidator(sessions)
	eventBus.Subscribe(func(m bus.Message) {
		if m.Kind == bus.KindSessionRevoked {
			sessions.Invalidate(m.User)
		}
	})
	users := user.NewHandler(userRepo, tokens, userRepo, cfg.RefreshTokenTTL)
	users.SetRevoker(hub)

	// Wire each feature package's handler onto a single router, behind the
	// Phase-5 middleware bundle (hardening, observability, rate limiting).
	mw, stop := buildMiddleware(cfg, hub, pgBus)
	defer stop()
	handler := api.NewRouter(
		users,
		room.NewHandler(room.NewPostgresRepository(db), hub),
		message.NewHandler(messageRepo, hub),
		ws.NewHandler(hub, messageRepo, userRepo, cfg.Security.CORSAllowedOrigins...),
		tokens,
		readiness{db: db, bus: pgBus},
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
func buildMiddleware(cfg config.Config, hub *ws.Hub, pgBus *bus.Postgres) (api.Middleware, func()) {
	var mw api.Middleware
	var closers []func()

	// Observability: one registry feeds both the request instrumentation and
	// the /metrics endpoint.
	var reg *metrics.Registry
	if cfg.Observability.MetricsEnabled {
		reg = metrics.New()
		mw.Metrics = reg.Handler()
		reg.GaugeFunc("vili_ws_connections", "Live websocket connections.", func() float64 {
			c, _ := hub.Stats()
			return float64(c)
		})
		reg.GaugeFunc("vili_ws_active_rooms", "Rooms with at least one live websocket connection.", func() float64 {
			_, r := hub.Stats()
			return float64(r)
		})
		if pgBus != nil {
			reg.GaugeFunc("vili_bus_connected", "1 when this instance's cluster bus listener is connected.", func() float64 {
				if pgBus.Connected() {
					return 1
				}
				return 0
			})
			reg.GaugeFunc("vili_bus_published", "Bus messages published by this instance since start.", func() float64 {
				p, _ := pgBus.Stats()
				return float64(p)
			})
			reg.GaugeFunc("vili_bus_received", "Bus messages received from other instances since start.", func() float64 {
				_, r := pgBus.Stats()
				return float64(r)
			})
		}
	}

	// Rate limiting: per-IP on auth, per-user on message POSTs.
	if cfg.RateLimit.Enabled {
		authLim := ratelimit.New(cfg.RateLimit.AuthRPS, cfg.RateLimit.AuthBurst)
		msgLim := ratelimit.New(cfg.RateLimit.MessagesRPS, cfg.RateLimit.MessagesBurst)
		closers = append(closers, authLim.Stop, msgLim.Stop)
		trusted, err := httputil.ParseTrustedProxies(cfg.Security.TrustedProxies)
		if err != nil {
			slog.Error("invalid TRUSTED_PROXIES", "error", err) // unreachable: config validates
		}
		mw.AuthLimit = httputil.RateLimit(authLim, httputil.NewKeyByIP(trusted))
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

// readiness reports ready only when the database answers and, in cluster
// mode, the bus listener is connected (otherwise this instance would serve
// stale realtime state, so the load balancer should route around it).
type readiness struct {
	db  *sql.DB
	bus *bus.Postgres
}

func (r readiness) PingContext(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		return err
	}
	if r.bus != nil && !r.bus.Connected() {
		return errors.New("realtime bus disconnected")
	}
	return nil
}
