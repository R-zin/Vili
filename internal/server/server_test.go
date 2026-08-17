package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/R-zin/vili/internal/config"
)

func testConfig(port int) config.Config {
	return config.Config{
		Port: port,
		HTTP: config.HTTP{
			ReadTimeout:       time.Second,
			ReadHeaderTimeout: time.Second,
			WriteTimeout:      time.Second,
			IdleTimeout:       time.Second,
			ShutdownTimeout:   2 * time.Second,
		},
	}
}

// freePort asks the OS for an available port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestRun_GracefulShutdownOnContextCancel(t *testing.T) {
	port := freePort(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, testConfig(port), handler) }()

	// Wait for the server to come up, then cancel to trigger graceful shutdown.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRun_ListenError(t *testing.T) {
	// Occupy the wildcard address the same way Run does so binding fails fast.
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	busyPort := l.Addr().(*net.TCPAddr).Port

	if err := Run(context.Background(), testConfig(busyPort), http.NotFoundHandler()); err == nil {
		t.Fatal("expected an error when the port is already in use")
	}
}
