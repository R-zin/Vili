// Package config loads process configuration from environment variables.
//
// Configuration is environment-only (os.LookupEnv); no external config lib.
// The typed Config is built once in main and injected everywhere it is
// needed; nothing re-reads the environment at request or call time.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	defaultPort             = 8080
	defaultJWTExpiryMinutes = 60
	defaultRefreshTokenTTL  = 30 * 24 * time.Hour // 30 days

	defaultHTTPReadTimeout       = 15 * time.Second
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPWriteTimeout      = 15 * time.Second
	defaultHTTPIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout       = 10 * time.Second

	defaultDBMaxOpenConns    = 25
	defaultDBMaxIdleConns    = 25
	defaultDBConnMaxLifetime = 5 * time.Minute

	defaultMaxBodyBytes = 1 << 20 // 1 MiB

	defaultRateLimitEnabled       = true
	defaultRateLimitAuthRPS       = 5
	defaultRateLimitAuthBurst     = 10
	defaultRateLimitMessagesRPS   = 10
	defaultRateLimitMessagesBurst = 20

	defaultWSMaxConnsPerUser = 5

	defaultSecurityHeadersEnabled = true
	defaultCORSAllowedOrigins     = ""

	defaultRequestLogEnabled = true
	defaultMetricsEnabled    = true
	defaultPprofEnabled      = false
)

// HTTP holds the timeouts for the HTTP server. Zero values are never used;
// every field is populated from defaults or the environment.
type HTTP struct {
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// DB holds the database/sql connection-pool settings.
type DB struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Security holds the hardening knobs: body-size caps, security headers, and
// the explicit CORS allowlist.
type Security struct {
	// MaxBodyBytes bounds every JSON request body (http.MaxBytesReader).
	MaxBodyBytes int64
	// HeadersEnabled toggles the security-headers middleware.
	HeadersEnabled bool
	// CORSAllowedOrigins is the explicit cross-origin allowlist. Empty means
	// deny-by-default: no cross-origin browser requests are permitted (the CLI
	// sends no Origin header and the index.html demo is opened as a local
	// file, so neither needs a CORS grant). Entries are scheme://host[:port]
	// origins.
	CORSAllowedOrigins []string
}

// RateLimit holds the token-bucket limiter knobs. RPS is the sustained refill
// rate; Burst is the maximum tokens a bucket can hold.
type RateLimit struct {
	Enabled bool
	// AuthPerIP limits /v1/auth/* by client IP.
	AuthRPS   float64
	AuthBurst int
	// Messages limits message POSTs by authenticated user.
	MessagesRPS   float64
	MessagesBurst int
}

// WS holds the realtime hardening knobs.
type WS struct {
	// MaxConnsPerUser caps concurrent live websocket connections per user.
	MaxConnsPerUser int
}

// Observability holds the metrics, request-logging, and pprof knobs.
type Observability struct {
	// RequestLogEnabled toggles per-request access logging via slog.
	RequestLogEnabled bool
	// MetricsEnabled toggles the /metrics endpoint (Prometheus text format).
	MetricsEnabled bool
	// PprofEnabled mounts net/http/pprof under /debug/pprof. Defaults to off;
	// enable only in a trusted environment.
	PprofEnabled bool
}

// Config is the immutable process configuration.
type Config struct {
	// Port the HTTP server listens on.
	Port int
	// DatabaseURL is the PostgreSQL connection string (required).
	DatabaseURL string
	// JWTSecret signs HS256 tokens (required, never empty).
	JWTSecret []byte
	// JWTExpiry is how long an issued access token remains valid.
	JWTExpiry time.Duration
	// RefreshTokenTTL is how long a refresh token remains valid before it must
	// be rotated or the user re-logs in.
	RefreshTokenTTL time.Duration

	HTTP          HTTP
	DB            DB
	Security      Security
	RateLimit     RateLimit
	WS            WS
	Observability Observability
}

// Load reads configuration from the environment and returns a validated
// Config. It fails fast if DATABASE_URL or JWT_SECRET is missing or empty.
func Load() (Config, error) {
	var cfg Config

	dbURL, ok := os.LookupEnv("DATABASE_URL")
	if !ok || dbURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required and must not be empty")
	}
	cfg.DatabaseURL = dbURL

	secret, ok := os.LookupEnv("JWT_SECRET")
	if !ok || secret == "" {
		return Config{}, fmt.Errorf("config: JWT_SECRET is required and must not be empty")
	}
	cfg.JWTSecret = []byte(secret)

	var err error
	if cfg.Port, err = intEnv("PORT", defaultPort); err != nil {
		return Config{}, err
	}
	expiryMinutes, err := intEnv("JWT_EXPIRY_MINUTES", defaultJWTExpiryMinutes)
	if err != nil {
		return Config{}, err
	}
	if expiryMinutes <= 0 {
		return Config{}, fmt.Errorf("config: JWT_EXPIRY_MINUTES must be positive, got %d", expiryMinutes)
	}
	cfg.JWTExpiry = time.Duration(expiryMinutes) * time.Minute

	if cfg.RefreshTokenTTL, err = durationEnv("REFRESH_TOKEN_TTL", defaultRefreshTokenTTL); err != nil {
		return Config{}, err
	}
	if cfg.RefreshTokenTTL <= 0 {
		return Config{}, fmt.Errorf("config: REFRESH_TOKEN_TTL must be positive, got %s", cfg.RefreshTokenTTL)
	}

	if cfg.HTTP.ReadTimeout, err = durationEnv("HTTP_READ_TIMEOUT", defaultHTTPReadTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTP.ReadHeaderTimeout, err = durationEnv("HTTP_READ_HEADER_TIMEOUT", defaultHTTPReadHeaderTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTP.WriteTimeout, err = durationEnv("HTTP_WRITE_TIMEOUT", defaultHTTPWriteTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTP.IdleTimeout, err = durationEnv("HTTP_IDLE_TIMEOUT", defaultHTTPIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.HTTP.ShutdownTimeout, err = durationEnv("SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return Config{}, err
	}

	if cfg.DB.MaxOpenConns, err = intEnv("DB_MAX_OPEN_CONNS", defaultDBMaxOpenConns); err != nil {
		return Config{}, err
	}
	if cfg.DB.MaxIdleConns, err = intEnv("DB_MAX_IDLE_CONNS", defaultDBMaxIdleConns); err != nil {
		return Config{}, err
	}
	if cfg.DB.ConnMaxLifetime, err = durationEnv("DB_CONN_MAX_LIFETIME", defaultDBConnMaxLifetime); err != nil {
		return Config{}, err
	}

	if err := cfg.loadSecurity(); err != nil {
		return Config{}, err
	}
	if err := cfg.loadRateLimit(); err != nil {
		return Config{}, err
	}
	if err := cfg.loadWS(); err != nil {
		return Config{}, err
	}
	if err := cfg.loadObservability(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// loadSecurity populates the hardening knobs.
func (cfg *Config) loadSecurity() error {
	maxBody, err := int64Env("MAX_BODY_BYTES", defaultMaxBodyBytes)
	if err != nil {
		return err
	}
	if maxBody <= 0 {
		return fmt.Errorf("config: MAX_BODY_BYTES must be positive, got %d", maxBody)
	}
	cfg.Security.MaxBodyBytes = maxBody

	if cfg.Security.HeadersEnabled, err = boolEnv("SECURITY_HEADERS_ENABLED", defaultSecurityHeadersEnabled); err != nil {
		return err
	}
	cfg.Security.CORSAllowedOrigins = listEnv("CORS_ALLOWED_ORIGINS", defaultCORSAllowedOrigins)
	return nil
}

// loadRateLimit populates the token-bucket limiter knobs.
func (cfg *Config) loadRateLimit() error {
	var err error
	if cfg.RateLimit.Enabled, err = boolEnv("RATE_LIMIT_ENABLED", defaultRateLimitEnabled); err != nil {
		return err
	}
	if cfg.RateLimit.AuthRPS, err = floatEnv("RATE_LIMIT_AUTH_RPS", defaultRateLimitAuthRPS); err != nil {
		return err
	}
	if cfg.RateLimit.AuthBurst, err = intEnv("RATE_LIMIT_AUTH_BURST", defaultRateLimitAuthBurst); err != nil {
		return err
	}
	if cfg.RateLimit.MessagesRPS, err = floatEnv("RATE_LIMIT_MESSAGES_RPS", defaultRateLimitMessagesRPS); err != nil {
		return err
	}
	if cfg.RateLimit.MessagesBurst, err = intEnv("RATE_LIMIT_MESSAGES_BURST", defaultRateLimitMessagesBurst); err != nil {
		return err
	}
	if cfg.RateLimit.Enabled {
		if cfg.RateLimit.AuthRPS <= 0 || cfg.RateLimit.AuthBurst <= 0 ||
			cfg.RateLimit.MessagesRPS <= 0 || cfg.RateLimit.MessagesBurst <= 0 {
			return fmt.Errorf("config: rate limit rates and bursts must be positive (got auth rps=%v burst=%d, messages rps=%v burst=%d)",
				cfg.RateLimit.AuthRPS, cfg.RateLimit.AuthBurst, cfg.RateLimit.MessagesRPS, cfg.RateLimit.MessagesBurst)
		}
	}
	return nil
}

// loadWS populates the realtime hardening knobs.
func (cfg *Config) loadWS() error {
	n, err := intEnv("WS_MAX_CONNS_PER_USER", defaultWSMaxConnsPerUser)
	if err != nil {
		return err
	}
	if n <= 0 {
		return fmt.Errorf("config: WS_MAX_CONNS_PER_USER must be positive, got %d", n)
	}
	cfg.WS.MaxConnsPerUser = n
	return nil
}

// loadObservability populates the metrics, logging, and pprof knobs.
func (cfg *Config) loadObservability() error {
	var err error
	if cfg.Observability.RequestLogEnabled, err = boolEnv("REQUEST_LOG_ENABLED", defaultRequestLogEnabled); err != nil {
		return err
	}
	if cfg.Observability.MetricsEnabled, err = boolEnv("METRICS_ENABLED", defaultMetricsEnabled); err != nil {
		return err
	}
	if cfg.Observability.PprofEnabled, err = boolEnv("PPROF_ENABLED", defaultPprofEnabled); err != nil {
		return err
	}
	return nil
}

// intEnv reads an integer environment variable, falling back to def when the
// variable is unset. An explicitly set but unparsable value is an error.
func intEnv(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q", key, raw)
	}
	return v, nil
}

// int64Env is intEnv for int64 values (byte-size limits).
func int64Env(key string, def int64) (int64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q", key, raw)
	}
	return v, nil
}

// floatEnv reads a float environment variable (a rate), falling back to def
// when unset. An explicitly set but unparsable value is an error.
func floatEnv(key string, def float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a number, got %q", key, raw)
	}
	return v, nil
}

// boolEnv reads a boolean environment variable ("true"/"false"/"1"/"0"),
// falling back to def when unset. An explicitly set but unparsable value is an
// error.
func boolEnv(key string, def bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: %s must be a boolean, got %q", key, raw)
	}
	return v, nil
}

// listEnv reads a comma-separated environment variable into a slice of trimmed,
// non-empty entries. An unset or empty value yields nil (e.g. an empty CORS
// allowlist = deny-by-default).
func listEnv(key, def string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		raw = def
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// durationEnv reads a duration environment variable (Go duration syntax such
// as "15s" or "5m"), falling back to def when unset. An explicitly set but
// unparsable value is an error.
func durationEnv(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration (e.g. \"15s\"), got %q", key, raw)
	}
	return v, nil
}
