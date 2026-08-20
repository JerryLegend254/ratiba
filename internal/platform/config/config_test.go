package config_test

import (
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/JerryLegend254/ratiba/internal/platform/config"
)

// A syntactically valid connection string for tests. Not a real credential;
// nothing ever connects with it.
const validDatabaseURL = "postgres://ratiba:secret@localhost:5432/ratiba?sslmode=disable" //nolint:gosec // test fixture

// setEnv applies a set of variables for the duration of the test. t.Setenv
// restores them afterwards and forbids parallel execution, which is what we
// want when mutating process-wide state.
func setEnv(t *testing.T, values map[string]string) {
	t.Helper()
	// Clear everything the loader reads so a variable left over from the
	// developer's shell cannot change the result.
	for _, key := range []string{
		"APP_ENV", "SERVICE_NAME", "PORT", "DATABASE_URL",
		"HTTP_READ_HEADER_TIMEOUT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT", "HTTP_HANDLER_TIMEOUT", "HTTP_SHUTDOWN_TIMEOUT",
		"HTTP_MAX_BODY_BYTES", "DB_MAX_CONNS", "DB_MIN_CONNS", "DB_MAX_CONN_LIFETIME",
		"DB_MAX_CONN_IDLE_TIME", "DB_CONNECT_TIMEOUT", "DB_STATEMENT_TIMEOUT",
		"LOG_LEVEL", "LOG_FORMAT",
		"BOOKING_MIN_LEAD_TIME", "PAGE_SIZE_DEFAULT", "PAGE_SIZE_MAX",
	} {
		t.Setenv(key, "")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": validDatabaseURL})

	cfg, err := config.Load(config.BuildInfo{Version: "v1", Commit: "abc"})
	if err != nil {
		t.Fatalf("expected the default configuration to load, got %v", err)
	}

	if cfg.Env != config.EnvDevelopment {
		t.Errorf("expected development by default, got %s", cfg.Env)
	}
	if cfg.HTTP.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.HTTP.Port)
	}
	if cfg.Booking.MinLeadTime != time.Hour {
		t.Errorf("expected a one hour lead time, got %s", cfg.Booking.MinLeadTime)
	}
	// Local development gets readable logs; deployed environments do not.
	if cfg.Logging.Format != "text" {
		t.Errorf("expected text logs in development, got %s", cfg.Logging.Format)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	setEnv(t, nil)

	_, err := config.Load(config.BuildInfo{})
	if err == nil {
		t.Fatal("expected startup to fail without DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("the error should name the missing variable, got: %v", err)
	}
}

// TestLoadReportsEveryProblemAtOnce checks the batching behaviour: an operator
// fixing a broken deployment should see all the mistakes in one go.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL": validDatabaseURL,
		"PORT":         "not-a-number",
		"LOG_LEVEL":    "verbose",
		"LOG_FORMAT":   "xml",
	})

	_, err := config.Load(config.BuildInfo{})
	if err == nil {
		t.Fatal("expected the configuration to be rejected")
	}

	message := err.Error()
	for _, expected := range []string{"PORT", "LOG_LEVEL", "LOG_FORMAT"} {
		if !strings.Contains(message, expected) {
			t.Errorf("expected the error to mention %s, got: %v", expected, err)
		}
	}
}

// TestProductionSafetyRules covers the fail-closed checks. Each of these would
// be a real incident if it slipped through.
func TestProductionSafetyRules(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			"APP_ENV":      "production",
			"DATABASE_URL": validDatabaseURL,
			"LOG_FORMAT":   "json",
		}
	}

	t.Run("a valid production configuration loads", func(t *testing.T) {
		setEnv(t, base())
		if _, err := config.Load(config.BuildInfo{}); err != nil {
			t.Fatalf("expected the configuration to load, got %v", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantKey string
	}{
		{
			name:    "logs must be machine parseable",
			mutate:  func(env map[string]string) { env["LOG_FORMAT"] = "text" },
			wantKey: "LOG_FORMAT",
		},
		{
			name:    "debug logging is refused",
			mutate:  func(env map[string]string) { env["LOG_LEVEL"] = "debug" },
			wantKey: "LOG_LEVEL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := base()
			tc.mutate(env)
			setEnv(t, env)

			_, err := config.Load(config.BuildInfo{})
			if err == nil {
				t.Fatal("expected production to refuse this configuration")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("expected the error to name %s, got: %v", tc.wantKey, err)
			}
		})
	}
}

func TestRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantKey string
	}{
		{
			name:    "non-postgres database URL",
			env:     map[string]string{"DATABASE_URL": "mysql://user:pass@localhost:3306/ratiba"}, //nolint:gosec // test fixture
			wantKey: "DATABASE_URL",
		},
		{
			name:    "database URL without a host",
			env:     map[string]string{"DATABASE_URL": "postgres:///ratiba"},
			wantKey: "DATABASE_URL",
		},
		{
			name:    "minimum connections above the maximum",
			env:     map[string]string{"DB_MIN_CONNS": "50", "DB_MAX_CONNS": "10"},
			wantKey: "DB_MIN_CONNS",
		},
		{
			name:    "default page size above the maximum",
			env:     map[string]string{"PAGE_SIZE_DEFAULT": "500", "PAGE_SIZE_MAX": "100"},
			wantKey: "PAGE_SIZE_DEFAULT",
		},
		{
			name: "handler timeout at or above the write timeout",
			// The timeout response could never be written.
			env:     map[string]string{"HTTP_HANDLER_TIMEOUT": "30s", "HTTP_WRITE_TIMEOUT": "20s"},
			wantKey: "HTTP_HANDLER_TIMEOUT",
		},
		{
			name:    "unknown environment",
			env:     map[string]string{"APP_ENV": "prod"},
			wantKey: "APP_ENV",
		},
		{
			name:    "port out of range",
			env:     map[string]string{"PORT": "70000"},
			wantKey: "PORT",
		},
		{
			name:    "non-duration timeout",
			env:     map[string]string{"HTTP_READ_TIMEOUT": "15"},
			wantKey: "HTTP_READ_TIMEOUT",
		},
		{
			name:    "body limit below the floor",
			env:     map[string]string{"HTTP_MAX_BODY_BYTES": "10"},
			wantKey: "HTTP_MAX_BODY_BYTES",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": validDatabaseURL}
			maps.Copy(env, tc.env)
			setEnv(t, env)

			_, err := config.Load(config.BuildInfo{})
			if err == nil {
				t.Fatal("expected the configuration to be rejected")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("expected the error to name %s, got: %v", tc.wantKey, err)
			}
		})
	}
}

// TestSecretsAreNeverLogged is the check that matters most in this file: a
// database password must not reach a log aggregator.
func TestSecretsAreNeverLogged(t *testing.T) {
	const password = "sup3r-s3cret-passw0rd"

	setEnv(t, map[string]string{
		"DATABASE_URL": "postgres://ratiba:" + password + "@db.internal:5432/ratiba",
	})

	cfg, err := config.Load(config.BuildInfo{Version: "v1", Commit: "abc"})
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}

	t.Run("the redacted URL keeps the host but drops the password", func(t *testing.T) {
		redacted := cfg.Database.RedactedURL()
		if strings.Contains(redacted, password) {
			t.Fatalf("the redacted URL leaked the password: %s", redacted)
		}
		if !strings.Contains(redacted, "db.internal") {
			t.Errorf("the redacted URL should keep the host for diagnostics, got %s", redacted)
		}
	})

	t.Run("the slog representation contains no secrets", func(t *testing.T) {
		var builder strings.Builder
		logger := slog.New(slog.NewJSONHandler(&builder, nil))
		logger.Info("startup", slog.Any("config", cfg))

		output := builder.String()
		if strings.Contains(output, password) {
			t.Errorf("the log line leaked the database password: %s", output)
		}
		// It should still carry the operational facts worth having.
		for _, expected := range []string{"db.internal", "http_port"} {
			if !strings.Contains(output, expected) {
				t.Errorf("expected the log line to include %q, got: %s", expected, output)
			}
		}
	})
}

func TestDatabaseURLFromEnv(t *testing.T) {
	t.Run("returns the configured URL", func(t *testing.T) {
		setEnv(t, map[string]string{"DATABASE_URL": validDatabaseURL})
		got, err := config.DatabaseURLFromEnv()
		if err != nil {
			t.Fatalf("expected the URL, got %v", err)
		}
		if got != validDatabaseURL {
			t.Errorf("expected %s, got %s", validDatabaseURL, got)
		}
	})

	t.Run("reports a missing URL", func(t *testing.T) {
		setEnv(t, nil)
		if _, err := config.DatabaseURLFromEnv(); err == nil {
			t.Fatal("expected an error when DATABASE_URL is unset")
		}
	})
}
