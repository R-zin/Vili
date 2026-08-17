// Command server is the Vili backend's only HTTP entrypoint. It loads config,
// connects to Postgres, runs migrations, wires the feature packages onto one
// router, and serves until interrupted.
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/R-zin/vili/internal/api"
	"github.com/R-zin/vili/internal/auth"
	"github.com/R-zin/vili/internal/config"
	"github.com/R-zin/vili/internal/message"
	"github.com/R-zin/vili/internal/room"
	"github.com/R-zin/vili/internal/server"
	"github.com/R-zin/vili/internal/store"
	"github.com/R-zin/vili/internal/user"
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

	// Wire each feature package's handler onto a single router.
	handler := api.NewRouter(
		user.NewHandler(user.NewPostgresRepository(db), tokens),
		room.NewHandler(room.NewPostgresRepository(db)),
		message.NewHandler(message.NewPostgresRepository(db)),
		tokens,
		db,
	)

	return server.Run(context.Background(), cfg, handler)
}
