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

// Run binds cfg.Port, serves handler, and blocks until an interrupt/SIGTERM
// arrives or the parent context is cancelled, then shuts down gracefully
// within cfg.HTTP.ShutdownTimeout. It returns nil on a clean shutdown and a
// non-nil error if binding or serving fails for any reason other than
// http.ErrServerClosed. Binding happens synchronously so an unusable address
// fails fast instead of racing the shutdown path.
func Run(ctx context.Context, cfg config.Config, handler http.Handler) error {
	addr := net.JoinHostPort("", strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", addr, err)
	}

	srv := &http.Server{
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
		slog.Info("http server listening", "addr", listener.Addr().String())
		serveErr <- srv.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections")
	case err := <-serveErr:
		// Serve returned before any shutdown signal.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server: serve: %w", err)
		}
		return nil
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("server: shutdown: %w", err)
	}

	// Wait for Serve to return after shutdown.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: serve: %w", err)
	}
	slog.Info("http server stopped cleanly")
	return nil
}
