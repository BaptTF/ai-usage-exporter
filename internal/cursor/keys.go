package cursor

import (
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// keyEnvPrefix is the only accepted API-key form: one variable per
	// subscription. A bare CURSOR_API_KEY is deliberately not discovered.
	keyEnvPrefix = "CURSOR_API_KEY_"

	// refreshEnvPrefix is the optional OAuth refresh form per subscription.
	refreshEnvPrefix = "CURSOR_REFRESH_TOKEN_"
	// legacyRefreshEnv is the historical OAuth refresh variable. It belongs to
	// the renamed historical credential, now subscription "r_personal".
	legacyRefreshEnv   = "CURSOR_REFRESH_TOKEN"
	legacyRefreshLabel = "r_personal"
)

// KeyConfig holds one Cursor credential and its bounded metric label.
//
// Every credential becomes an independent provider instance: its own auth,
// plan, quotas, spend, price and collection state.
type KeyConfig struct {
	// EnvVar names the variable that provides APIKey. For a refresh-only
	// credential it names the refresh-token variable instead.
	EnvVar string
	// RefreshTokenEnvVar names the OAuth refresh-token variable when set.
	RefreshTokenEnvVar string
	Label              string
	APIKey             string
	RefreshToken       string
}

// LoadKeysFromEnv discovers Cursor credentials from the process environment.
func LoadKeysFromEnv() []KeyConfig {
	return LoadKeys(os.Environ())
}

// LoadKeys discovers every non-empty CURSOR_API_KEY_<suffix> credential and
// labels it with the sanitized suffix: CURSOR_API_KEY_B_PERSONAL becomes
// subscription "b_personal". A bare CURSOR_API_KEY is ignored; there is no
// "default" Cursor subscription.
//
// CURSOR_REFRESH_TOKEN is attached only to "r_personal", the renamed
// historical base credential. CURSOR_REFRESH_TOKEN_<suffix> is attached to the
// matching suffixed credential, or creates a refresh-only credential when no
// API key exists for that label.
func LoadKeys(environ []string) []KeyConfig {
	keys := make([]KeyConfig, 0)
	refreshes := make([]KeyConfig, 0)

	for _, envEntry := range environ {
		name, raw, ok := strings.Cut(envEntry, "=")
		if !ok {
			continue
		}
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}

		if suffix, ok := envSuffix(name, keyEnvPrefix); ok {
			keys = append(keys, KeyConfig{
				EnvVar: name,
				Label:  sanitizeLabel(suffix),
				APIKey: value,
			})
			continue
		}

		if name == legacyRefreshEnv {
			refreshes = append(refreshes, KeyConfig{
				EnvVar:             name,
				RefreshTokenEnvVar: name,
				Label:              legacyRefreshLabel,
				RefreshToken:       value,
			})
			continue
		}

		if suffix, ok := envSuffix(name, refreshEnvPrefix); ok {
			refreshes = append(refreshes, KeyConfig{
				EnvVar:             name,
				RefreshTokenEnvVar: name,
				Label:              sanitizeLabel(suffix),
				RefreshToken:       value,
			})
		}
	}

	for _, refresh := range refreshes {
		attached := false
		for i := range keys {
			if keys[i].Label == refresh.Label {
				keys[i].RefreshToken = refresh.RefreshToken
				keys[i].RefreshTokenEnvVar = refresh.RefreshTokenEnvVar
				attached = true
				break
			}
		}
		if !attached {
			keys = append(keys, refresh)
		}
	}

	// Sort by variable name for a deterministic provider/subscription order.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].EnvVar != keys[j].EnvVar {
			return keys[i].EnvVar < keys[j].EnvVar
		}
		return keys[i].Label < keys[j].Label
	})
	return keys
}

// ClientConfig builds the per-credential auth configuration. The OAuth refresh
// token is used only for the credential it was discovered for.
func (k KeyConfig) ClientConfig(timeout time.Duration) Config {
	apiVar := ""
	if k.APIKey != "" {
		apiVar = k.EnvVar
	}
	return Config{
		APIKey:          k.APIKey,
		RefreshToken:    k.RefreshToken,
		APIKeyVar:       apiVar,
		RefreshTokenVar: k.RefreshTokenEnvVar,
		Timeout:         timeout,
	}
}

// AuthModeLabel describes the credential for startup logs. It never includes
// the secret value.
func (k KeyConfig) AuthModeLabel() string {
	return k.ClientConfig(0).AuthModeLabel()
}

// envSuffix matches PREFIX followed by a non-empty suffix. It deliberately
// does not match the bare prefix or unrelated names such as
// CURSOR_API_KEYSTORE.
func envSuffix(name, prefix string) (string, bool) {
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" {
		return "", false
	}
	return suffix, true
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
