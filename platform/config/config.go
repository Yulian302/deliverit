// Package config loads service configuration from environment variables
// (12-factor style) into typed structs.
//
// Each service declares a struct that embeds Common and adds its own fields:
//
//	type Config struct {
//		config.Common
//		DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
//	}
//
// Tags: `env:"NAME"` picks the variable, `envDefault:"..."` sets a default,
// `required` fails when the variable is unset, `notEmpty` fails when it is set
// but empty. Values are converted to the field's type ("20s" → time.Duration).
package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Common holds the settings every DeliverIt service has.
type Common struct {
	// ServiceName identifies the service in logs, traces and metrics.
	ServiceName string `env:"SERVICE_NAME,required,notEmpty"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`
	// HTTPAddr is the listen address for the service's HTTP server.
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// OTLPEndpoint is the OpenTelemetry collector (host:port). Empty disables export.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	// ShutdownGrace bounds how long graceful shutdown may take.
	ShutdownGrace time.Duration `env:"SHUTDOWN_GRACE" envDefault:"20s"`
}

// Load fills dst (a pointer to a struct) from the environment.
// The error reports every invalid or missing variable at once, always by its
// environment variable name — that is what an operator can actually fix.
func Load(dst any) error {
	err := env.Parse(dst)
	if err == nil {
		return nil
	}

	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return fmt.Errorf("load config: %w", err)
	}

	keys := envKeysByField(reflect.TypeOf(dst))
	msgs := make([]string, 0, len(agg.Errors))
	for _, e := range agg.Errors {
		var pe env.ParseError
		if errors.As(e, &pe) {
			// ParseError names the Go field ("ShutdownGrace"); translate it.
			msgs = append(msgs, fmt.Sprintf("%s: invalid value: %v", keys[pe.Name], pe.Err))
			continue
		}
		// Missing/empty errors already name the variable.
		msgs = append(msgs, e.Error())
	}
	return fmt.Errorf("load config: %s", strings.Join(msgs, "; "))
}

// envKeysByField maps struct field names to their env variable names,
// descending into embedded and nested structs.
func envKeysByField(t reflect.Type) map[string]string {
	keys := map[string]string{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for f := range t.Fields() {
			if name, _, _ := strings.Cut(f.Tag.Get("env"), ","); name != "" {
				keys[f.Name] = name
			}
			walk(f.Type)
		}
	}
	walk(t)
	return keys
}
