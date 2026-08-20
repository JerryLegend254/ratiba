// Package config loads and validates Ratiba's configuration from the
// environment.
//
// Configuration is read exactly once, at startup, into an immutable struct.
// Nothing else in the codebase reads os.Getenv. That means a misconfigured
// deployment fails immediately with a complete list of problems, instead of
// panicking hours later the first time some rarely-taken code path looks up a
// variable that was never set.
//
// Validation fails closed. Where a wrong value would be merely suboptimal the
// loader substitutes a documented default; where a wrong value would be unsafe
// it refuses to start.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment names a deployment tier.
type Environment string

const (
	// EnvDevelopment is a developer machine or the shared development tier.
	EnvDevelopment Environment = "development"
	// EnvStaging is the pre-production release validation tier.
	EnvStaging Environment = "staging"
	// EnvProduction is the public deployment.
	EnvProduction Environment = "production"
	// EnvTest is used by automated tests.
	EnvTest Environment = "test"
)

// IsProduction reports whether this environment is internet-facing and holds
// real data. Several safety checks key off it.
func (e Environment) IsProduction() bool { return e == EnvProduction }

// Valid reports whether e is a known environment.
func (e Environment) Valid() bool {
	switch e {
	case EnvDevelopment, EnvStaging, EnvProduction, EnvTest:
		return true
	default:
		return false
	}
}

// Config is the fully validated application configuration.
type Config struct {
	Env         Environment
	ServiceName string

	// Build carries values injected at link time. They are not read from the
	// environment.
	Build BuildInfo

	HTTP     HTTPConfig
	Database DatabaseConfig
	Logging  LoggingConfig
	Booking  BookingConfig
}

// BuildInfo identifies the running binary. Injected with -ldflags; see the
// Makefile and Dockerfile.
type BuildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

// HTTPConfig tunes the API server.
type HTTPConfig struct {
	// Port is the listening port. Railway injects PORT.
	Port int
	// ReadHeaderTimeout bounds how long a client may take to send headers. It
	// is the specific defence against a Slowloris-style connection hold.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// HandlerTimeout bounds a single request's processing. It must stay below
	// WriteTimeout so the timeout response can actually be written.
	HandlerTimeout time.Duration
	// ShutdownTimeout bounds draining on SIGTERM.
	ShutdownTimeout time.Duration
	// MaxRequestBodyBytes caps a request body. Ratiba's largest legitimate body
	// is a few hundred bytes.
	MaxRequestBodyBytes int64
}

// DatabaseConfig tunes PostgreSQL connectivity.
type DatabaseConfig struct {
	// URL is a secret and must never be logged. Use RedactedURL for output.
	URL string
	// MaxConns should be sized from the environment's connection budget
	// divided by the replica count, not copied from a tutorial.
	MaxConns int32
	MinConns int32

	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
	// StatementTimeout is applied server-side to every session, so a runaway
	// query cannot hold a connection indefinitely even if a context leaks.
	StatementTimeout time.Duration
}

// RedactedURL returns the database URL with any password replaced, safe for
// logs and error messages.
func (d DatabaseConfig) RedactedURL() string {
	parsed, err := url.Parse(d.URL)
	if err != nil {
		return "postgres://<unparseable>"
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), "xxxxx")
		}
	}
	parsed.RawQuery = ""
	return parsed.String()
}

// LoggingConfig controls log output.
type LoggingConfig struct {
	Level slog.Level
	// Format is "json" or "text". Deployed environments are forced to json.
	Format string
}

// BookingConfig carries the domain's tunables.
//
// Slot duration is deliberately absent. It is fixed at 30 minutes by a CHECK
// constraint on the appointments table, so exposing it as an environment
// variable would offer a setting that silently breaks every write. Changing it
// is a migration plus a code change, not a redeploy. The domain's Policy type
// still parameterises it so unit tests can exercise other durations.
type BookingConfig struct {
	MinLeadTime     time.Duration
	IdempotencyTTL  time.Duration
	DefaultPageSize int32
	MaxPageSize     int32
}

// Load reads, defaults and validates configuration from the process
// environment.
//
// Every problem found is reported together: an operator fixing a deployment
// should not have to restart five times to discover five mistakes.
func Load(build BuildInfo) (Config, error) {
	p := &parser{}

	cfg := Config{
		Env:         Environment(p.str("APP_ENV", string(EnvDevelopment))),
		ServiceName: p.str("SERVICE_NAME", "ratiba-api"),
		Build:       build,
	}

	if !cfg.Env.Valid() {
		p.fail("APP_ENV", fmt.Sprintf("must be one of development, staging, production, test (got %q)", cfg.Env))
		// Continue with a safe assumption so the remaining checks still run and
		// report everything at once.
		cfg.Env = EnvDevelopment
	}

	cfg.HTTP = HTTPConfig{
		Port:                p.intRange("PORT", 8080, 1, 65535),
		ReadHeaderTimeout:   p.duration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:         p.duration("HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:        p.duration("HTTP_WRITE_TIMEOUT", 20*time.Second),
		IdleTimeout:         p.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
		HandlerTimeout:      p.duration("HTTP_HANDLER_TIMEOUT", 10*time.Second),
		ShutdownTimeout:     p.duration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
		MaxRequestBodyBytes: p.int64Range("HTTP_MAX_BODY_BYTES", 64*1024, 1024, 8*1024*1024),
	}

	cfg.Database = DatabaseConfig{
		URL:              p.required("DATABASE_URL"),
		MaxConns:         p.int32Range("DB_MAX_CONNS", 10, 1, 500),
		MinConns:         p.int32Range("DB_MIN_CONNS", 2, 0, 500),
		MaxConnLifetime:  p.duration("DB_MAX_CONN_LIFETIME", 30*time.Minute),
		MaxConnIdleTime:  p.duration("DB_MAX_CONN_IDLE_TIME", 5*time.Minute),
		ConnectTimeout:   p.duration("DB_CONNECT_TIMEOUT", 5*time.Second),
		StatementTimeout: p.duration("DB_STATEMENT_TIMEOUT", 8*time.Second),
	}

	cfg.Logging = LoggingConfig{
		Level:  p.logLevel("LOG_LEVEL", slog.LevelInfo),
		Format: strings.ToLower(p.str("LOG_FORMAT", defaultLogFormat(cfg.Env))),
	}

	cfg.Booking = BookingConfig{
		MinLeadTime:     p.duration("BOOKING_MIN_LEAD_TIME", time.Hour),
		DefaultPageSize: p.int32Range("PAGE_SIZE_DEFAULT", 20, 1, 1000),
		MaxPageSize:     p.int32Range("PAGE_SIZE_MAX", 100, 1, 1000),
	}

	validate(p, &cfg)

	if err := p.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// defaultLogFormat keeps local development readable while guaranteeing machine
// parseable logs anywhere a log aggregator might be watching.
func defaultLogFormat(env Environment) string {
	if env == EnvDevelopment {
		return "text"
	}
	return "json"
}

// validate applies the cross-field and safety rules that a per-variable parser
// cannot express.
func validate(p *parser, cfg *Config) {
	if cfg.Database.URL != "" {
		parsed, err := url.Parse(cfg.Database.URL)
		switch {
		case err != nil:
			p.fail("DATABASE_URL", "must be a valid URL")
		case parsed.Scheme != "postgres" && parsed.Scheme != "postgresql":
			p.fail("DATABASE_URL", "must use the postgres:// or postgresql:// scheme")
		case parsed.Host == "":
			p.fail("DATABASE_URL", "must include a host")
		}
	}

	if cfg.Database.MinConns > cfg.Database.MaxConns {
		p.fail("DB_MIN_CONNS", "must not exceed DB_MAX_CONNS")
	}

	if cfg.Booking.DefaultPageSize > cfg.Booking.MaxPageSize {
		p.fail("PAGE_SIZE_DEFAULT", "must not exceed PAGE_SIZE_MAX")
	}
	if cfg.Booking.MinLeadTime < 0 {
		p.fail("BOOKING_MIN_LEAD_TIME", "must not be negative")
	}

	switch cfg.Logging.Format {
	case "json", "text":
	default:
		p.fail("LOG_FORMAT", `must be "json" or "text"`)
	}

	// A handler that outlives the write deadline can never deliver its own
	// timeout response, so the client sees a dropped connection instead of a
	// 503. Catch the misconfiguration rather than debug it in production.
	if cfg.HTTP.HandlerTimeout >= cfg.HTTP.WriteTimeout {
		p.fail("HTTP_HANDLER_TIMEOUT", "must be shorter than HTTP_WRITE_TIMEOUT so the timeout response can be written")
	}

	if cfg.Env.IsProduction() {
		if cfg.Logging.Format != "json" {
			p.fail("LOG_FORMAT", `must be "json" in production so logs are machine parseable`)
		}
		if cfg.Logging.Level < slog.LevelInfo {
			p.fail("LOG_LEVEL", "must be info or higher in production to avoid logging request detail at scale")
		}
	}
}

// LogValue renders the configuration for the startup log line with every secret
// removed. slog calls this automatically, so a Config can never be logged raw.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", string(c.Env)),
		slog.String("service", c.ServiceName),
		slog.String("version", c.Build.Version),
		slog.String("commit", c.Build.Commit),
		slog.Int("http_port", c.HTTP.Port),
		slog.Duration("http_handler_timeout", c.HTTP.HandlerTimeout),
		slog.Duration("http_shutdown_timeout", c.HTTP.ShutdownTimeout),
		slog.Int64("http_max_body_bytes", c.HTTP.MaxRequestBodyBytes),
		slog.String("database", c.Database.RedactedURL()),
		slog.Int("db_max_conns", int(c.Database.MaxConns)),
		slog.Int("db_min_conns", int(c.Database.MinConns)),
		slog.String("log_level", c.Logging.Level.String()),
		slog.String("log_format", c.Logging.Format),
		slog.Duration("min_lead_time", c.Booking.MinLeadTime),
	)
}

// parser accumulates configuration problems instead of returning at the first
// one.
type parser struct {
	problems []string
}

func (p *parser) fail(key, message string) {
	p.problems = append(p.problems, fmt.Sprintf("%s: %s", key, message))
}

func (p *parser) err() error {
	if len(p.problems) == 0 {
		return nil
	}
	sort.Strings(p.problems)
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(p.problems, "\n  - "))
}

// lookup returns the trimmed value of key and whether it was set to anything
// non-empty.
func lookup(key string) (string, bool) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(raw)
	return trimmed, trimmed != ""
}

func (p *parser) str(key, fallback string) string {
	if value, ok := lookup(key); ok {
		return value
	}
	return fallback
}

func (p *parser) required(key string) string {
	value, ok := lookup(key)
	if !ok {
		p.fail(key, "is required")
		return ""
	}
	return value
}

func (p *parser) intRange(key string, fallback, minimum, maximum int) int {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		p.fail(key, "must be an integer")
		return fallback
	}
	if value < minimum || value > maximum {
		p.fail(key, fmt.Sprintf("must be between %d and %d", minimum, maximum))
		return fallback
	}
	return value
}

// int32Range parses a bounded int32. It exists so callers that need an int32
// never have to write a conversion the compiler cannot prove is safe: the
// bounds are checked before the value is narrowed.
func (p *parser) int32Range(key string, fallback, minimum, maximum int32) int32 {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		p.fail(key, "must be an integer")
		return fallback
	}
	narrowed := int32(value)
	if narrowed < minimum || narrowed > maximum {
		p.fail(key, fmt.Sprintf("must be between %d and %d", minimum, maximum))
		return fallback
	}
	return narrowed
}

func (p *parser) int64Range(key string, fallback, minimum, maximum int64) int64 {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		p.fail(key, "must be an integer")
		return fallback
	}
	if value < minimum || value > maximum {
		p.fail(key, fmt.Sprintf("must be between %d and %d", minimum, maximum))
		return fallback
	}
	return value
}

func (p *parser) float(key string, fallback float64) float64 {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		p.fail(key, "must be a number")
		return fallback
	}
	return value
}

func (p *parser) boolean(key string, fallback bool) bool {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		p.fail(key, "must be a boolean (true/false)")
		return fallback
	}
	return value
}

func (p *parser) duration(key string, fallback time.Duration) time.Duration {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		p.fail(key, `must be a Go duration such as "30s", "5m" or "1h"`)
		return fallback
	}
	if value <= 0 {
		p.fail(key, "must be positive")
		return fallback
	}
	return value
}

func (p *parser) logLevel(key string, fallback slog.Level) slog.Level {
	raw, ok := lookup(key)
	if !ok {
		return fallback
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		p.fail(key, "must be one of debug, info, warn, error")
		return fallback
	}
	return level
}

// csv splits a comma-separated list, dropping empty entries.
func (p *parser) csv(key string) []string {
	raw, ok := lookup(key)
	if !ok {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

// ErrMissingDatabaseURL is returned by helper commands that need only the
// database connection string.
var ErrMissingDatabaseURL = errors.New("DATABASE_URL is required")

// DatabaseURLFromEnv reads just the database URL, for the migrate command,
// which has no need for the full application configuration.
func DatabaseURLFromEnv() (string, error) {
	value, ok := lookup("DATABASE_URL")
	if !ok {
		return "", ErrMissingDatabaseURL
	}
	return value, nil
}
