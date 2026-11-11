package opencode

import (
	"os"
	"sort"
	"strings"
)

const keyPrefix = "OPENCODE_GO_API_KEY"

// KeyConfig holds an API key and its bounded metric label.
type KeyConfig struct {
	EnvVar string
	Label  string
	Key    string
}

// LoadKeysFromEnv discovers all OpenCode Go API keys from the process
// environment. Any variable named OPENCODE_GO_API_KEY or starting with
// OPENCODE_GO_API_KEY_ is loaded; unset or blank values are ignored.
//
// Examples:
//
//	OPENCODE_GO_API_KEY=sk-...       -> label "main"
//	OPENCODE_GO_API_KEY_R=sk-...     -> label "r"
//	OPENCODE_GO_API_KEY_ALICE=sk-... -> label "alice"
func LoadKeysFromEnv() []KeyConfig {
	return LoadKeys(os.Environ())
}

// LoadKeys is the testable form of LoadKeysFromEnv.
func LoadKeys(environ []string) []KeyConfig {
	var keys []KeyConfig
	for _, envEntry := range environ {
		name, val, ok := strings.Cut(envEntry, "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}

		suffix, ok := keySuffix(name)
		if !ok {
			continue
		}

		label := "main"
		if suffix != "" {
			label = sanitizeLabel(suffix)
		}
		keys = append(keys, KeyConfig{
			EnvVar: name,
			Label:  label,
			Key:    val,
		})
	}

	// Sort by env var name for a deterministic subscription order.
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].EnvVar < keys[j].EnvVar
	})
	return keys
}

// keySuffix matches the exact base variable or the underscore-suffixed form.
// It deliberately does not match unrelated names such as
// OPENCODE_GO_API_KEYSTORE.
func keySuffix(name string) (string, bool) {
	if name == keyPrefix {
		return "", true
	}
	if strings.HasPrefix(name, keyPrefix+"_") {
		return strings.TrimPrefix(name, keyPrefix+"_"), true
	}
	return "", false
}

// sanitizeLabel keeps metric labels bounded and free of surprising bytes:
// lower-case ASCII letters and digits are kept, everything else becomes '_'.
// The result is truncated to 64 characters.
func sanitizeLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		out = "key"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
