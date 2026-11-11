package config

import (
	"strings"
	"testing"
	"time"
)

func envMap(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func toEnviron(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}

func load(values map[string]string) (Config, error) {
	return LoadFrom(envMap(values), toEnviron(values))
}

func TestDefaultsWithOneOpenCodeKey(t *testing.T) {
	cfg, err := load(map[string]string{"OPENCODE_GO_API_KEY": "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d", cfg.Port)
	}
	if cfg.PollInterval != 5*time.Minute {
		t.Errorf("PollInterval = %s", cfg.PollInterval)
	}
	if cfg.RequestTimeout != 15*time.Second {
		t.Errorf("RequestTimeout = %s", cfg.RequestTimeout)
	}
	if len(cfg.OpenCodeKeys) != 1 || cfg.OpenCodeKeys[0].Label != "main" {
		t.Errorf("keys = %+v", cfg.OpenCodeKeys)
	}
	if cfg.CursorEnabled() || cfg.DevinEnabled() {
		t.Error("optional providers enabled unexpectedly")
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := load(map[string]string{
		"OPENCODE_GO_API_KEY": "sk-test",
		"PORT":                "9090",
		"POLL_INTERVAL":       "30s",
		"REQUEST_TIMEOUT":     "2s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 || cfg.PollInterval != 30*time.Second || cfg.RequestTimeout != 2*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestOpenCodeZeroKeysWithCursorIsAllowed(t *testing.T) {
	cfg, err := load(map[string]string{"CURSOR_API_KEY": "crsr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenCodeEnabled() || !cfg.CursorEnabled() {
		t.Errorf("cfg = %+v", cfg)
	}
	if got := cfg.Cursor.AuthModeLabel(); !strings.Contains(got, "CURSOR_API_KEY") {
		t.Errorf("AuthModeLabel = %q", got)
	}
}

func TestOpenCodeZeroKeysWithDevinIsAllowed(t *testing.T) {
	cfg, err := load(map[string]string{"DEVIN_API_KEY": "cog_test"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenCodeEnabled() || !cfg.DevinEnabled() {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestNoProviderIsRejected(t *testing.T) {
	_, err := load(map[string]string{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no provider configured") {
		t.Errorf("error = %q", err)
	}
}

func TestInvalidPort(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-1", "65536", "80.5"} {
		t.Run(raw, func(t *testing.T) {
			_, err := load(map[string]string{"OPENCODE_GO_API_KEY": "sk", "PORT": raw})
			if err == nil || !strings.Contains(err.Error(), "PORT") {
				t.Errorf("error = %v, want PORT error", err)
			}
		})
	}
}

func TestInvalidDurations(t *testing.T) {
	cases := []struct{ name, value string }{
		{"POLL_INTERVAL", "abc"},
		{"POLL_INTERVAL", "0s"},
		{"POLL_INTERVAL", "-5s"},
		{"REQUEST_TIMEOUT", "abc"},
		{"REQUEST_TIMEOUT", "0"},
		{"REQUEST_TIMEOUT", "-1s"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.value, func(t *testing.T) {
			_, err := load(map[string]string{"OPENCODE_GO_API_KEY": "sk", tc.name: tc.value})
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error = %v, want %s error", err, tc.name)
			}
		})
	}
}

func TestDuplicateSubscriptionLabelsRejectedWithoutValues(t *testing.T) {
	_, err := load(map[string]string{
		"OPENCODE_GO_API_KEY":      "sk-first-secret",
		"OPENCODE_GO_API_KEY_MAIN": "sk-second-secret",
	})
	if err == nil {
		t.Fatal("expected duplicate label error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate") || !strings.Contains(msg, "OPENCODE_GO_API_KEY") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "sk-first-secret") || strings.Contains(msg, "sk-second-secret") {
		t.Errorf("error leaks key values: %q", msg)
	}
}

func TestCursorAndDevinEnvironmentParsing(t *testing.T) {
	cfg, err := load(map[string]string{
		"DEVIN_API_KEY":    "  cog_test\n",
		"DEVIN_ORG_ID":     " org-123 ",
		"DEVIN_RESET_DAY":  "12",
		"DEVIN_API_SERVER": " https://seat.example ",
		"CURSOR_API_KEY":   " crsr_test ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Devin.Token != "cog_test" || cfg.Devin.OrgID != "org-123" || cfg.Devin.ResetDay != 12 {
		t.Errorf("devin = %+v", cfg.Devin)
	}
	if cfg.Devin.SeatURL != "https://seat.example" {
		t.Errorf("SeatURL = %q", cfg.Devin.SeatURL)
	}
	if cfg.Cursor.APIKey != "crsr_test" {
		t.Errorf("Cursor APIKey = %q", cfg.Cursor.APIKey)
	}
}

func TestInvalidDevinResetDayFallsBack(t *testing.T) {
	cfg, err := load(map[string]string{"DEVIN_API_KEY": "cog", "DEVIN_RESET_DAY": "31"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Devin.ResetDay != 5 {
		t.Errorf("ResetDay = %d, want 5", cfg.Devin.ResetDay)
	}
}

func TestSecretsAreDeduplicatedAndComplete(t *testing.T) {
	cfg, err := load(map[string]string{
		"OPENCODE_GO_API_KEY":       "opencode-secret",
		"OPENCODE_GO_API_KEY_ALICE": "alice-secret",
		"CURSOR_API_KEY":            "cursor-secret",
		"DEVIN_API_KEY":             "devin-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := cfg.Secrets()
	want := map[string]bool{
		"opencode-secret": true,
		"alice-secret":    true,
		"cursor-secret":   true,
		"devin-secret":    true,
	}
	if len(secrets) != len(want) {
		t.Fatalf("secrets = %v", secrets)
	}
	for _, s := range secrets {
		if !want[s] {
			t.Errorf("unexpected secret %q", s)
		}
	}
}

func TestSecretsIgnoreEmptyValues(t *testing.T) {
	cfg, err := load(map[string]string{
		"OPENCODE_GO_API_KEY": "sk",
		"DEVIN_API_KEY":       "   ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Secrets()) != 1 {
		t.Errorf("secrets = %v", cfg.Secrets())
	}
}
