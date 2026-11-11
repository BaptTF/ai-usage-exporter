// Package config loads and validates the exporter configuration from the
// environment. Invalid values fail startup with a message that names the
// variable, never its value.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/cursor"
	"github.com/BaptTF/ai-usage-exporter/internal/devin"
	"github.com/BaptTF/ai-usage-exporter/internal/opencode"
)

const (
	defaultPort           = 8080
	defaultPollInterval   = 5 * time.Minute
	defaultRequestTimeout = 15 * time.Second
)

// Config is the validated runtime configuration.
type Config struct {
	Port           int
	PollInterval   time.Duration
	RequestTimeout time.Duration

	OpenCodeKeys []opencode.KeyConfig
	Cursor       cursor.Config
	Devin        devin.Config
}

// Load reads the process environment.
func Load() (Config, error) {
	return LoadFrom(os.Getenv, os.Environ())
}

// LoadFrom is the testable loader. getenv supplies scalar variables, environ
// supplies the full list used for dynamic OpenCode key discovery.
func LoadFrom(getenv func(string) string, environ []string) (Config, error) {
	port, err := parsePort(getenv("PORT"))
	if err != nil {
		return Config{}, err
	}
	poll, err := parsePositiveDuration("POLL_INTERVAL", getenv("POLL_INTERVAL"), defaultPollInterval)
	if err != nil {
		return Config{}, err
	}
	timeout, err := parsePositiveDuration("REQUEST_TIMEOUT", getenv("REQUEST_TIMEOUT"), defaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}

	keys := opencode.LoadKeys(environ)
	if err := validateKeys(keys); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Port:           port,
		PollInterval:   poll,
		RequestTimeout: timeout,
		OpenCodeKeys:   keys,
		Cursor: cursor.Config{
			APIKey:       strings.TrimSpace(getenv("CURSOR_API_KEY")),
			RefreshToken: strings.TrimSpace(getenv("CURSOR_REFRESH_TOKEN")),
			Timeout:      timeout,
		},
		Devin: devin.Config{
			Token:    strings.TrimSpace(getenv("DEVIN_API_KEY")),
			OrgID:    strings.TrimSpace(getenv("DEVIN_ORG_ID")),
			ResetDay: devin.ResetDayFromValue(getenv("DEVIN_RESET_DAY")),
			Timeout:  timeout,
			SeatURL:  strings.TrimSpace(getenv("DEVIN_API_SERVER")),
		},
	}

	if len(cfg.OpenCodeKeys) == 0 && !cfg.Cursor.Enabled() && cfg.Devin.Token == "" {
		return Config{}, fmt.Errorf(
			"no provider configured: set OPENCODE_GO_API_KEY (or a suffixed variant), CURSOR_API_KEY/CURSOR_REFRESH_TOKEN, or DEVIN_API_KEY")
	}
	return cfg, nil
}

// OpenCodeEnabled reports whether at least one OpenCode key is configured.
func (c Config) OpenCodeEnabled() bool { return len(c.OpenCodeKeys) > 0 }

// CursorEnabled reports whether a Cursor credential is configured.
func (c Config) CursorEnabled() bool { return c.Cursor.Enabled() }

// DevinEnabled reports whether a Devin token is configured.
func (c Config) DevinEnabled() bool { return c.Devin.Token != "" }

// Secrets returns every credential value for redaction before logging.
// Values are deduplicated; empty strings are ignored.
func (c Config) Secrets() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, k := range c.OpenCodeKeys {
		add(k.Key)
	}
	add(c.Cursor.APIKey)
	add(c.Cursor.RefreshToken)
	add(c.Devin.Token)
	return out
}

func parsePort(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultPort, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid PORT: want an integer between 1 and 65535")
	}
	return port, nil
}

func parsePositiveDuration(name, raw string, fallback time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s: want a positive Go duration such as 30s or 5m", name)
	}
	return d, nil
}

// validateKeys rejects duplicate metric labels, which would create duplicate
// Prometheus series. The error names the environment variables, not values.
func validateKeys(keys []opencode.KeyConfig) error {
	seen := make(map[string]string, len(keys))
	for _, k := range keys {
		if prev, ok := seen[k.Label]; ok {
			return fmt.Errorf("duplicate OpenCode subscription label %q from %s and %s", k.Label, prev, k.EnvVar)
		}
		seen[k.Label] = k.EnvVar
	}
	return nil
}
