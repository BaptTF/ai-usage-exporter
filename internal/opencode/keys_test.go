package opencode

import "testing"

func findKey(keys []KeyConfig, label string) (KeyConfig, bool) {
	for _, k := range keys {
		if k.Label == label {
			return k, true
		}
	}
	return KeyConfig{}, false
}

func TestLoadKeysDynamicDiscovery(t *testing.T) {
	keys := LoadKeys([]string{
		"OPENCODE_GO_API_KEY=base-secret",
		"OPENCODE_GO_API_KEY_R=r-secret",
		"OPENCODE_GO_API_KEY_Alice=alice-secret",
		"UNRELATED=value",
	})
	if len(keys) != 3 {
		t.Fatalf("len(keys) = %d, want 3: %+v", len(keys), keys)
	}
	for _, tc := range []struct{ label, key string }{
		{"main", "base-secret"},
		{"r", "r-secret"},
		{"alice", "alice-secret"},
	} {
		k, ok := findKey(keys, tc.label)
		if !ok {
			t.Errorf("missing label %q in %+v", tc.label, keys)
			continue
		}
		if k.Key != tc.key {
			t.Errorf("label %q key = %q, want %q", tc.label, k.Key, tc.key)
		}
	}
}

func TestLoadKeysIgnoresBlanksAndLookalikes(t *testing.T) {
	keys := LoadKeys([]string{
		"OPENCODE_GO_API_KEY=",
		"OPENCODE_GO_API_KEY_SPACE=   ",
		"OPENCODE_GO_API_KEYSTORE=not-a-key",
		"XOPENCODE_GO_API_KEY=x",
	})
	if len(keys) != 0 {
		t.Fatalf("keys = %+v, want none", keys)
	}
}

func TestLoadKeysNoKeys(t *testing.T) {
	if got := LoadKeys(nil); len(got) != 0 {
		t.Fatalf("keys = %+v, want none", got)
	}
}

func TestLoadKeysSortsByEnvVarAndSanitizesLabel(t *testing.T) {
	keys := LoadKeys([]string{
		"OPENCODE_GO_API_KEY_Z=last",
		"OPENCODE_GO_API_KEY_A_B=alpha",
	})
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	if keys[0].EnvVar != "OPENCODE_GO_API_KEY_A_B" || keys[1].EnvVar != "OPENCODE_GO_API_KEY_Z" {
		t.Errorf("order = %q, %q", keys[0].EnvVar, keys[1].EnvVar)
	}

	weird := LoadKeys([]string{"OPENCODE_GO_API_KEY_Ünicode Secret=value"})
	if len(weird) != 1 {
		t.Fatalf("weird = %+v", weird)
	}
	if weird[0].Label != "nicode_secret" {
		t.Errorf("sanitized label = %q, want nicode_secret", weird[0].Label)
	}
}

func TestSanitizeLabelBounds(t *testing.T) {
	long := ""
	for i := 0; i < 100; i++ {
		long += "A"
	}
	got := sanitizeLabel(long)
	if len(got) != 64 {
		t.Errorf("len(label) = %d, want 64", len(got))
	}
	if got := sanitizeLabel("___"); got != "key" {
		t.Errorf("sanitizeLabel(___ ) = %q, want key", got)
	}
}
