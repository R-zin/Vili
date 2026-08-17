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
	"time"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	defaultPort             = 8080
	defaultJWTExpiryMinutes = 60

	defaultHTTPReadTimeout       = 15 * time.Second
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultHTTPWriteTimeout      = 15 * time.Second
	defaultHTTPIdleTimeout       = 60 * time.Second
	defaultShutdownTimeout       = 10 * time.Second

	defaultDBMaxOpenConns    = 25
	defaultDBMaxIdleConns    = 25
	defaultDBConnMaxLifetime = 5 * time.Minute
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

// Config is the immutable process configuration.
type Config struct {
	// Port the HTTP server listens on.
	Port int
	// DatabaseURL is the PostgreSQL connection string (required).
	DatabaseURL string
	// JWTSecret signs HS256 tokens (required, never empty).
	JWTSecret []byte
	// JWTExpiry is how long an issued token remains valid.
	JWTExpiry time.Duration

	HTTP HTTP
	DB   DB
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

	return cfg, nil
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
