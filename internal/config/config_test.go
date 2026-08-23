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
		"MAX_BODY_BYTES", "SECURITY_HEADERS_ENABLED", "CORS_ALLOWED_ORIGINS",
		"RATE_LIMIT_ENABLED", "RATE_LIMIT_AUTH_RPS", "RATE_LIMIT_AUTH_BURST",
		"RATE_LIMIT_MESSAGES_RPS", "RATE_LIMIT_MESSAGES_BURST",
		"WS_MAX_CONNS_PER_USER",
		"REQUEST_LOG_ENABLED", "METRICS_ENABLED", "PPROF_ENABLED",
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

func TestLoad_Phase5Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Security.MaxBodyBytes != 1<<20 {
		t.Errorf("MaxBodyBytes = %d, want 1 MiB", cfg.Security.MaxBodyBytes)
	}
	if !cfg.Security.HeadersEnabled {
		t.Error("expected security headers on by default")
	}
	if cfg.Security.CORSAllowedOrigins != nil {
		t.Errorf("expected deny-by-default CORS (nil), got %v", cfg.Security.CORSAllowedOrigins)
	}
	if !cfg.RateLimit.Enabled {
		t.Error("expected rate limiting on by default")
	}
	if cfg.RateLimit.AuthRPS != 5 || cfg.RateLimit.AuthBurst != 10 {
		t.Errorf("auth limit = %v/%d", cfg.RateLimit.AuthRPS, cfg.RateLimit.AuthBurst)
	}
	if cfg.RateLimit.MessagesRPS != 10 || cfg.RateLimit.MessagesBurst != 20 {
		t.Errorf("messages limit = %v/%d", cfg.RateLimit.MessagesRPS, cfg.RateLimit.MessagesBurst)
	}
	if cfg.WS.MaxConnsPerUser != 5 {
		t.Errorf("MaxConnsPerUser = %d, want 5", cfg.WS.MaxConnsPerUser)
	}
	if !cfg.Observability.RequestLogEnabled || !cfg.Observability.MetricsEnabled {
		t.Errorf("expected request-log and metrics on by default, got %+v", cfg.Observability)
	}
	if cfg.Observability.PprofEnabled {
		t.Error("expected pprof off by default")
	}
}

func TestLoad_CORSAllowlist(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("CORS_ALLOWED_ORIGINS", "https://app.example.com, https://admin.example.com ,")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://app.example.com", "https://admin.example.com"}
	if len(cfg.Security.CORSAllowedOrigins) != len(want) {
		t.Fatalf("CORSAllowedOrigins = %v, want %v", cfg.Security.CORSAllowedOrigins, want)
	}
	for i := range want {
		if cfg.Security.CORSAllowedOrigins[i] != want[i] {
			t.Errorf("origin[%d] = %q, want %q", i, cfg.Security.CORSAllowedOrigins[i], want[i])
		}
	}
}

func TestLoad_InvalidPhase5Values(t *testing.T) {
	cases := []struct {
		name, key, val string
	}{
		{"max body not int", "MAX_BODY_BYTES", "huge"},
		{"max body negative", "MAX_BODY_BYTES", "-1"},
		{"auth rps not float", "RATE_LIMIT_AUTH_RPS", "lots"},
		{"auth burst not int", "RATE_LIMIT_AUTH_BURST", "x"},
		{"messages rps negative", "RATE_LIMIT_MESSAGES_RPS", "-2"},
		{"ws conns not int", "WS_MAX_CONNS_PER_USER", "many"},
		{"ws conns zero", "WS_MAX_CONNS_PER_USER", "0"},
		{"headers not bool", "SECURITY_HEADERS_ENABLED", "maybe"},
		{"rate enabled not bool", "RATE_LIMIT_ENABLED", "sure"},
		{"request log not bool", "REQUEST_LOG_ENABLED", "yep"},
		{"metrics not bool", "METRICS_ENABLED", "nope"},
		{"pprof not bool", "PPROF_ENABLED", "yeah"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://localhost/db")
			t.Setenv("JWT_SECRET", "secret")
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q", tc.key, tc.val)
			}
		})
	}
}

func TestLoad_RateLimitDisabledSkipsValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("RATE_LIMIT_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimit.Enabled {
		t.Error("expected rate limiting disabled")
	}
}
