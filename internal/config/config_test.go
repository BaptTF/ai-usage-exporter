package config

import (
	"strings"
	"testing"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/cursor"
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
	cfg, err := load(map[string]string{"OPENCODE_GO_API_KEY_B": "sk-test"})
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
	if len(cfg.OpenCodeKeys) != 1 || cfg.OpenCodeKeys[0].Label != "b" {
		t.Errorf("keys = %+v", cfg.OpenCodeKeys)
	}
	for _, k := range cfg.OpenCodeKeys {
		if k.Label == "main" {
			t.Errorf("base label main emitted without the base variable: %+v", k)
		}
	}
	if cfg.CursorEnabled() || cfg.DevinEnabled() {
		t.Error("optional providers enabled unexpectedly")
	}
}

func TestOpenCodeSuffixedKeysNeedNoBaseAndEmitNoMain(t *testing.T) {
	cfg, err := load(map[string]string{
		"OPENCODE_GO_API_KEY_B": "b-secret",
		"OPENCODE_GO_API_KEY_R": "r-secret",
		"OPENCODE_GO_API_KEY_A": "a-secret",
		"OPENCODE_GO_API_KEY_N": "n-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a": true, "b": true, "n": true, "r": true}
	if len(cfg.OpenCodeKeys) != len(want) {
		t.Fatalf("keys = %+v", cfg.OpenCodeKeys)
	}
	for _, k := range cfg.OpenCodeKeys {
		if !want[k.Label] {
			t.Errorf("unexpected OpenCode label %q", k.Label)
		}
		if k.Label == "main" {
			t.Errorf("label main must not be emitted: %+v", k)
		}
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := load(map[string]string{
		"OPENCODE_GO_API_KEY_B": "sk-test",
		"PORT":                  "9090",
		"POLL_INTERVAL":         "30s",
		"REQUEST_TIMEOUT":       "2s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 9090 || cfg.PollInterval != 30*time.Second || cfg.RequestTimeout != 2*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestOpenCodeZeroKeysWithCursorIsAllowed(t *testing.T) {
	cfg, err := load(map[string]string{"CURSOR_API_KEY_R_PERSONAL": "crsr_test"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenCodeEnabled() || !cfg.CursorEnabled() {
		t.Errorf("cfg = %+v", cfg)
	}
	if len(cfg.CursorKeys) != 1 || cfg.CursorKeys[0].Label != "r_personal" {
		t.Fatalf("CursorKeys = %+v", cfg.CursorKeys)
	}
	if got := cfg.CursorKeys[0].AuthModeLabel(); !strings.Contains(got, "CURSOR_API_KEY_R_PERSONAL") {
		t.Errorf("AuthModeLabel = %q", got)
	}
}

func TestCursorSuffixedCredentialsEnableIndependentSubscriptions(t *testing.T) {
	cfg, err := load(map[string]string{
		"CURSOR_API_KEY_R_PERSONAL": "crsr_r_secret",
		"CURSOR_API_KEY_B_PERSONAL": "crsr_b_personal_secret",
		"CURSOR_API_KEY_B_STUDENT":  "crsr_b_student_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CursorEnabled() {
		t.Fatal("CursorEnabled = false")
	}
	want := map[string]string{
		"r_personal": "crsr_r_secret",
		"b_personal": "crsr_b_personal_secret",
		"b_student":  "crsr_b_student_secret",
	}
	if len(cfg.CursorKeys) != len(want) {
		t.Fatalf("CursorKeys = %+v", cfg.CursorKeys)
	}
	for _, key := range cfg.CursorKeys {
		wantValue, ok := want[key.Label]
		if !ok {
			t.Errorf("unexpected Cursor subscription %q", key.Label)
			continue
		}
		if key.APIKey != wantValue {
			t.Errorf("subscription %q has the wrong credential", key.Label)
		}
		if key.Label == "default" {
			t.Errorf("default subscription must not be emitted: %+v", key)
		}
	}
}

func TestCursorRefreshTokenOnlyAppliesToRPersonal(t *testing.T) {
	cfg, err := load(map[string]string{
		"CURSOR_API_KEY_R_PERSONAL":      "crsr_r_secret",
		"CURSOR_API_KEY_B_PERSONAL":      "crsr_b_secret",
		"CURSOR_REFRESH_TOKEN":           "legacy-refresh-secret",
		"CURSOR_REFRESH_TOKEN_B_STUDENT": "student-refresh-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	byLabel := make(map[string]cursor.KeyConfig, len(cfg.CursorKeys))
	for _, key := range cfg.CursorKeys {
		byLabel[key.Label] = key
	}
	if got := byLabel["r_personal"].RefreshToken; got != "legacy-refresh-secret" {
		t.Errorf("r_personal refresh token = %q", got)
	}
	if got := byLabel["b_personal"].RefreshToken; got != "" {
		t.Errorf("b_personal must not receive the legacy refresh token: %q", got)
	}
	if got := byLabel["b_student"].RefreshToken; got != "student-refresh-secret" {
		t.Errorf("b_student refresh token = %q", got)
	}
}

func TestCursorLegacyRefreshTokenCreatesRPersonalWithoutBaseAPIKey(t *testing.T) {
	cfg, err := load(map[string]string{
		"CURSOR_REFRESH_TOKEN":      "legacy-refresh-secret",
		"CURSOR_API_KEY_B_PERSONAL": "crsr_b_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CursorKeys) != 2 {
		t.Fatalf("CursorKeys = %+v", cfg.CursorKeys)
	}
	for _, key := range cfg.CursorKeys {
		if key.Label != "b_personal" && key.Label != "r_personal" {
			t.Errorf("unexpected label %q", key.Label)
		}
		if key.Label == "default" {
			t.Errorf("default subscription emitted: %+v", key)
		}
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
	if len(cfg.DevinKeys) != 1 || cfg.DevinKeys[0].Label != "r" {
		t.Errorf("DevinKeys = %+v", cfg.DevinKeys)
	}
}

func TestDevinSuffixedCredentialsBecomeIndependentSubscriptions(t *testing.T) {
	cfg, err := load(map[string]string{
		"DEVIN_API_KEY_B": "cog_b",
		"DEVIN_API_KEY_R": "cog_r",
		// The legacy bare key must not add a third "r" or a "default".
		"DEVIN_API_KEY": "cog_legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.DevinKeys) != 2 {
		t.Fatalf("DevinKeys = %+v", cfg.DevinKeys)
	}
	if cfg.DevinKeys[0].Label != "b" || cfg.DevinKeys[0].Token != "cog_b" {
		t.Errorf("first Devin key = %+v", cfg.DevinKeys[0])
	}
	if cfg.DevinKeys[1].Label != "r" || cfg.DevinKeys[1].Token != "cog_r" {
		t.Errorf("second Devin key = %+v", cfg.DevinKeys[1])
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
			_, err := load(map[string]string{"OPENCODE_GO_API_KEY_B": "sk", "PORT": raw})
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
			_, err := load(map[string]string{"OPENCODE_GO_API_KEY_B": "sk", tc.name: tc.value})
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error = %v, want %s error", err, tc.name)
			}
		})
	}
}

func TestDuplicateSubscriptionLabelsRejectedWithoutValues(t *testing.T) {
	_, err := load(map[string]string{
		"OPENCODE_GO_API_KEY_B": "sk-first-secret",
		"OPENCODE_GO_API_KEY_b": "sk-second-secret",
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

func TestDuplicateCursorSubscriptionLabelsRejectedWithoutValues(t *testing.T) {
	_, err := load(map[string]string{
		"CURSOR_API_KEY_B_PERSONAL": "crsr-first-secret",
		"CURSOR_API_KEY_b_personal": "crsr-second-secret",
	})
	if err == nil {
		t.Fatal("expected duplicate label error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "duplicate Cursor") || !strings.Contains(msg, "CURSOR_API_KEY_B_PERSONAL") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "crsr-first-secret") || strings.Contains(msg, "crsr-second-secret") {
		t.Errorf("error leaks key values: %q", msg)
	}
}

func TestBareCursorAPIKeyIsIgnored(t *testing.T) {
	cfg, err := load(map[string]string{
		"DEVIN_API_KEY":  "cog_test",
		"CURSOR_API_KEY": "crsr_legacy_base_secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CursorEnabled() {
		t.Errorf("bare CURSOR_API_KEY enabled Cursor: %+v", cfg.CursorKeys)
	}
}

func TestCursorAndDevinEnvironmentParsing(t *testing.T) {
	cfg, err := load(map[string]string{
		"DEVIN_API_KEY_R":           "  cog_test\n",
		"DEVIN_API_KEY_B":           "cog_b",
		"DEVIN_ORG_ID":              " org-123 ",
		"DEVIN_ORG_ID_R":            " org-r ",
		"DEVIN_RESET_DAY":           "12",
		"DEVIN_API_SERVER":          " https://seat.example ",
		"CURSOR_API_KEY_B_PERSONAL": " crsr_test ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.DevinKeys) != 2 {
		t.Fatalf("DevinKeys = %+v", cfg.DevinKeys)
	}
	b := cfg.DevinKeys[0]
	r := cfg.DevinKeys[1]
	if b.Label != "b" || b.Token != "cog_b" || b.OrgID != "org-123" {
		t.Errorf("Devin B = %+v", b)
	}
	if r.Label != "r" || r.Token != "cog_test" || r.OrgID != "org-r" {
		t.Errorf("Devin R = %+v", r)
	}
	if cfg.DevinResetDay != 12 {
		t.Errorf("DevinResetDay = %d", cfg.DevinResetDay)
	}
	if cfg.DevinSeatURL != "https://seat.example" {
		t.Errorf("SeatURL = %q", cfg.DevinSeatURL)
	}
	if len(cfg.CursorKeys) != 1 || cfg.CursorKeys[0].Label != "b_personal" {
		t.Fatalf("CursorKeys = %+v", cfg.CursorKeys)
	}
	if cfg.CursorKeys[0].APIKey != "crsr_test" {
		t.Errorf("Cursor APIKey = %q", cfg.CursorKeys[0].APIKey)
	}
}

func TestInvalidDevinResetDayFallsBack(t *testing.T) {
	cfg, err := load(map[string]string{"DEVIN_API_KEY_R": "cog", "DEVIN_RESET_DAY": "31"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DevinResetDay != 5 {
		t.Errorf("ResetDay = %d, want 5", cfg.DevinResetDay)
	}
}

func TestSecretsAreDeduplicatedAndComplete(t *testing.T) {
	cfg, err := load(map[string]string{
		"OPENCODE_GO_API_KEY_B":        "opencode-secret",
		"OPENCODE_GO_API_KEY_ALICE":    "alice-secret",
		"CURSOR_API_KEY_R_PERSONAL":    "cursor-r-secret",
		"CURSOR_API_KEY_B_PERSONAL":    "cursor-b-personal-secret",
		"CURSOR_API_KEY_B_STUDENT":     "cursor-b-student-secret",
		"CURSOR_REFRESH_TOKEN":         "cursor-refresh-secret",
		"CURSOR_REFRESH_TOKEN_STUDENT": "cursor-student-refresh-secret",
		"DEVIN_API_KEY_B":              "devin-b-secret",
		"DEVIN_API_KEY_R":              "devin-r-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	secrets := cfg.Secrets()
	want := map[string]bool{
		"opencode-secret":               true,
		"alice-secret":                  true,
		"cursor-r-secret":               true,
		"cursor-b-personal-secret":      true,
		"cursor-b-student-secret":       true,
		"cursor-refresh-secret":         true,
		"cursor-student-refresh-secret": true,
		"devin-b-secret":                true,
		"devin-r-secret":                true,
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
		"OPENCODE_GO_API_KEY_B": "sk",
		"DEVIN_API_KEY":         "   ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Secrets()) != 1 {
		t.Errorf("secrets = %v", cfg.Secrets())
	}
}
