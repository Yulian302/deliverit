package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Yulian302/deliverit/platform/config"
)

// testConfig mimics a real service config: Common plus a service-specific field.
type testConfig struct {
	config.Common
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
}

// unsetenv removes a variable for this test only (t.Setenv restores it after).
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_Defaults(t *testing.T) {
	t.Setenv("SERVICE_NAME", "x")

	var cfg config.Common
	if err := config.Load(&cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ServiceName != "x" {
		t.Errorf("ServiceName = %q, want x", cfg.ServiceName)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("OTLPEndpoint = %q, want empty", cfg.OTLPEndpoint)
	}
	if cfg.ShutdownGrace != 20*time.Second {
		t.Errorf("ShutdownGrace = %v, want 20s", cfg.ShutdownGrace)
	}
}

func TestLoad_ReadsValuesAndEmbeddedFields(t *testing.T) {
	t.Setenv("SERVICE_NAME", "order")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_GRACE", "5s")
	t.Setenv("DATABASE_URL", "postgres://x")

	var cfg testConfig
	if err := config.Load(&cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ServiceName != "order" || cfg.LogLevel != "debug" || cfg.ShutdownGrace != 5*time.Second {
		t.Errorf("common fields = %+v", cfg.Common)
	}
	if cfg.DatabaseURL != "postgres://x" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	// Make sure the variables are unset even if the developer's shell has them.
	unsetenv(t, "SERVICE_NAME")
	unsetenv(t, "DATABASE_URL")

	var cfg testConfig
	err := config.Load(&cfg)
	if err == nil {
		t.Fatal("Load: want error for missing required variables")
	}
	// Every missing variable is reported at once, not one per restart.
	for _, name := range []string{"SERVICE_NAME", "DATABASE_URL"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}

func TestLoad_EmptyServiceNameRejected(t *testing.T) {
	t.Setenv("SERVICE_NAME", "")

	var cfg config.Common
	err := config.Load(&cfg)
	if err == nil || !strings.Contains(err.Error(), "SERVICE_NAME") {
		t.Fatalf("Load error = %v, want one mentioning SERVICE_NAME", err)
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	t.Setenv("SERVICE_NAME", "x")
	t.Setenv("SHUTDOWN_GRACE", "twenty")

	var cfg config.Common
	err := config.Load(&cfg)
	if err == nil || !strings.Contains(err.Error(), "SHUTDOWN_GRACE") {
		t.Fatalf("Load error = %v, want one mentioning SHUTDOWN_GRACE", err)
	}
}
