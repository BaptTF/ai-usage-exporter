package main

import (
	"bytes"
	"log"
	"sort"
	"strings"
	"testing"

	"github.com/BaptTF/ai-usage-exporter/internal/config"
)

func loadConfig(t *testing.T, values map[string]string) config.Config {
	t.Helper()
	environ := make([]string, 0, len(values))
	for name, value := range values {
		environ = append(environ, name+"="+value)
	}
	cfg, err := config.LoadFrom(func(name string) string { return values[name] }, environ)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildProvidersWithCursorCredentialsOnly(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		"CURSOR_API_KEY_R_PERSONAL": "crsr_r_secret",
		"CURSOR_API_KEY_B_PERSONAL": "crsr_b_personal_secret",
		"CURSOR_API_KEY_B_STUDENT":  "crsr_b_student_secret",
	})

	providers := buildProviders(cfg)
	if len(providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(providers))
	}
	seen := make(map[string]bool)
	for _, p := range providers {
		if p.Name() != "cursor" {
			t.Errorf("provider name = %q, want cursor", p.Name())
		}
		subs := p.Subscriptions()
		if len(subs) != 1 {
			t.Errorf("provider subs = %v, want one", subs)
			continue
		}
		seen[subs[0]] = true
		if subs[0] == "default" {
			t.Error("default Cursor subscription emitted")
		}
	}
	for _, label := range []string{"b_personal", "b_student", "r_personal"} {
		if !seen[label] {
			t.Errorf("missing Cursor provider for %q: %v", label, seen)
		}
	}

	var logBuf bytes.Buffer
	logProviderSummary(log.New(&logBuf, "", 0), cfg, providers)
	logs := logBuf.String()
	for _, label := range []string{"b_personal", "b_student", "r_personal"} {
		if !strings.Contains(logs, "subscription="+label) {
			t.Errorf("startup logs do not name subscription %q: %s", label, logs)
		}
	}
	for _, secret := range []string{"crsr_r_secret", "crsr_b_personal_secret", "crsr_b_student_secret"} {
		if strings.Contains(logs, secret) {
			t.Errorf("startup logs leak a Cursor credential: %s", logs)
		}
	}
	if strings.Contains(logs, "subscription=default") {
		t.Errorf("startup logs mention a default Cursor subscription: %s", logs)
	}
}

func TestBuildProvidersWithDevinCredentialsOnly(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		"DEVIN_API_KEY_B": "cog_b_secret",
		"DEVIN_API_KEY_R": "cog_r_secret",
	})

	providers := buildProviders(cfg)
	if len(providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(providers))
	}
	seen := make(map[string]bool)
	for _, p := range providers {
		if p.Name() != "devin" {
			t.Errorf("provider name = %q, want devin", p.Name())
		}
		subs := p.Subscriptions()
		if len(subs) != 1 {
			t.Errorf("provider subs = %v, want one", subs)
			continue
		}
		seen[subs[0]] = true
		if subs[0] == "default" {
			t.Error("default Devin subscription emitted")
		}
	}
	for _, label := range []string{"b", "r"} {
		if !seen[label] {
			t.Errorf("missing Devin provider for %q: %v", label, seen)
		}
	}

	var logBuf bytes.Buffer
	logProviderSummary(log.New(&logBuf, "", 0), cfg, providers)
	logs := logBuf.String()
	for _, label := range []string{"b", "r"} {
		if !strings.Contains(logs, "subscription="+label) {
			t.Errorf("startup logs do not name subscription %q: %s", label, logs)
		}
	}
	for _, secret := range []string{"cog_b_secret", "cog_r_secret"} {
		if strings.Contains(logs, secret) {
			t.Errorf("startup logs leak a Devin credential: %s", logs)
		}
	}
	if strings.Contains(logs, "subscription=default") {
		t.Errorf("startup logs mention a default Devin subscription: %s", logs)
	}
}

func TestBuildProvidersWithSuffixedOpenCodeKeysEmitsNoMain(t *testing.T) {
	cfg := loadConfig(t, map[string]string{
		"OPENCODE_GO_API_KEY_B": "b-secret",
		"OPENCODE_GO_API_KEY_R": "r-secret",
		"OPENCODE_GO_API_KEY_A": "a-secret",
		"OPENCODE_GO_API_KEY_N": "n-secret",
	})

	providers := buildProviders(cfg)
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	if providers[0].Name() != "opencode" {
		t.Errorf("provider name = %q, want opencode", providers[0].Name())
	}
	subs := append([]string(nil), providers[0].Subscriptions()...)
	sort.Strings(subs)
	want := []string{"a", "b", "n", "r"}
	if strings.Join(subs, ",") != strings.Join(want, ",") {
		t.Errorf("subscriptions = %v, want %v", subs, want)
	}
	for _, sub := range subs {
		if sub == "main" {
			t.Error("label main emitted without the base OpenCode variable")
		}
	}
}
