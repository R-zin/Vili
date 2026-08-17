package config

import (
	"testing"
	"time"
)

// clearEnv unsets every variable Load reads so each test controls its own env.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"DATABASE_URL", "JWT_SECRET", "PORT", "JWT_EXPIRY_MINUTES",
		"HTTP_READ_TIMEOUT", "HTTP_READ_HEADER_TIMEOUT", "HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT", "SHUTDOWN_TIMEOUT",
		"DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
	} {
		t.Setenv(k, "")
	}
}

func TestLoad_RequiresDatabaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("JWT_SECRET", "x")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when DATABASE_URL is unset/empty")
	}
}

func TestLoad_RequiresJWTSecret(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when JWT_SECRET is unset/empty")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("default Port = %d, want 8080", cfg.Port)
	}
	if cfg.JWTExpiry != 60*time.Minute {
		t.Errorf("default JWTExpiry = %s, want 60m", cfg.JWTExpiry)
	}
	if cfg.DatabaseURL != "postgres://localhost/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if string(cfg.JWTSecret) != "secret" {
		t.Errorf("JWTSecret mismatch")
	}
	if cfg.HTTP.ReadTimeout == 0 || cfg.HTTP.WriteTimeout == 0 ||
		cfg.HTTP.IdleTimeout == 0 || cfg.HTTP.ReadHeaderTimeout == 0 ||
		cfg.HTTP.ShutdownTimeout == 0 {
		t.Errorf("expected all HTTP timeouts populated, got %+v", cfg.HTTP)
	}
	if cfg.DB.MaxOpenConns == 0 || cfg.DB.MaxIdleConns == 0 || cfg.DB.ConnMaxLifetime == 0 {
		t.Errorf("expected DB pool settings populated, got %+v", cfg.DB)
	}
}

func TestLoad_Overrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("PORT", "9090")
	t.Setenv("JWT_EXPIRY_MINUTES", "5")
	t.Setenv("HTTP_READ_TIMEOUT", "1s")
	t.Setenv("DB_MAX_OPEN_CONNS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.JWTExpiry != 5*time.Minute {
		t.Errorf("JWTExpiry = %s, want 5m", cfg.JWTExpiry)
	}
	if cfg.HTTP.ReadTimeout != time.Second {
		t.Errorf("ReadTimeout = %s, want 1s", cfg.HTTP.ReadTimeout)
	}
	if cfg.DB.MaxOpenConns != 7 {
		t.Errorf("MaxOpenConns = %d, want 7", cfg.DB.MaxOpenConns)
	}
}

func TestLoad_InvalidNumeric(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("PORT", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-integer PORT")
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("HTTP_READ_TIMEOUT", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-duration HTTP_READ_TIMEOUT")
	}
}

func TestLoad_NonPositiveExpiry(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("JWT_EXPIRY_MINUTES", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-positive JWT_EXPIRY_MINUTES")
	}
}
