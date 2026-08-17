// Package server runs the HTTP server with explicit timeouts and graceful
// shutdown. It owns no routing or business logic; it is given a composed
// handler and the process configuration.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/R-zin/vili/internal/config"
)

// Run starts the HTTP server on cfg.Port and serves handler until an
// interrupt/SIGTERM arrives, then shuts down gracefully within
// cfg.HTTP.ShutdownTimeout. It returns nil on a clean shutdown and a non-nil
// error if ListenAndServe fails for any reason other than http.ErrServerClosed.
func Run(ctx context.Context, cfg config.Config, handler http.Handler) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	// Cancel this context on SIGINT/SIGTERM to trigger shutdown.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	case err := <-serveErr:
		// Server stopped on its own before any signal.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: listen: %w", err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}

	// Wait for ListenAndServe to return after shutdown.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: listen: %w", err)
	}
	slog.Info("http server stopped cleanly")
	return nil
}
