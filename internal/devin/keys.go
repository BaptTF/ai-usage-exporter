package devin

import (
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// keyEnvPrefix is the canonical multi-credential form.
	keyEnvPrefix = "DEVIN_API_KEY_"

	// legacyKeyEnv is the historical single credential. It is treated as the
	// "r" subscription while the deployment migrates to DEVIN_API_KEY_R, and is
	// ignored as soon as that explicit variable exists.
	legacyKeyEnv   = "DEVIN_API_KEY"
	legacyKeyLabel = "r"
)

// KeyConfig holds one Devin credential and its bounded metric label.
type KeyConfig struct {
	EnvVar string
	Label  string
	Token  string
	OrgID  string
}

// LoadKeysFromEnv discovers Devin credentials from the process environment.
func LoadKeysFromEnv() []KeyConfig {
	return LoadKeys(os.Environ())
}

// LoadKeys discovers every non-empty DEVIN_API_KEY_<suffix> variable and labels
// it with the sanitized suffix: DEVIN_API_KEY_B becomes subscription "b".
// The bare DEVIN_API_KEY is accepted only while no DEVIN_API_KEY_R exists, and
// maps to "r" so the historical credential keeps its new account label during
// migration.
func LoadKeys(environ []string) []KeyConfig {
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, raw, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		values[name] = strings.TrimSpace(raw)
	}

	sharedOrg := values["DEVIN_ORG_ID"]
	keys := make([]KeyConfig, 0)
	hasR := false
	for name, value := range values {
		if value == "" {
			continue
		}
		suffix, ok := envSuffix(name, keyEnvPrefix)
		if !ok {
			continue
		}
		label := sanitizeLabel(suffix)
		if label == legacyKeyLabel {
			hasR = true
		}
		keys = append(keys, KeyConfig{
			EnvVar: name,
			Label:  label,
			Token:  value,
			OrgID:  orgFor(values, suffix, sharedOrg),
		})
	}

	if !hasR {
		if token := values[legacyKeyEnv]; token != "" {
			keys = append(keys, KeyConfig{
				EnvVar: legacyKeyEnv,
				Label:  legacyKeyLabel,
				Token:  token,
				OrgID:  orgFor(values, "R", sharedOrg),
			})
		}
	}

	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Label != keys[j].Label {
			return keys[i].Label < keys[j].Label
		}
		return keys[i].EnvVar < keys[j].EnvVar
	})
	return keys
}

// ClientConfig builds the client configuration for one Devin credential.
func (k KeyConfig) ClientConfig(timeout time.Duration, resetDay int, seatURL string) Config {
	return Config{
		Token:    k.Token,
		OrgID:    k.OrgID,
		ResetDay: resetDay,
		Timeout:  timeout,
		SeatURL:  seatURL,
	}
}

func orgFor(values map[string]string, suffix, shared string) string {
	if org := values["DEVIN_ORG_ID_"+strings.ToUpper(suffix)]; org != "" {
		return org
	}
	return shared
}

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
