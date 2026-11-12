package cursor

import "testing"

func findCursorKey(keys []KeyConfig, label string) (KeyConfig, bool) {
	for _, key := range keys {
		if key.Label == label {
			return key, true
		}
	}
	return KeyConfig{}, false
}

func TestLoadKeysDiscoversSuffixedCredentialsOnly(t *testing.T) {
	keys := LoadKeys([]string{
		"CURSOR_API_KEY_R_PERSONAL=crsr-r",
		"CURSOR_API_KEY_B_PERSONAL=crsr-b-personal",
		"CURSOR_API_KEY_B_STUDENT=crsr-b-student",
		"CURSOR_API_KEY=crsr-bare-legacy",
		"CURSOR_API_KEYSTORE=lookalike",
		"XCURSOR_API_KEY_B=x",
		"UNRELATED=value",
	})
	if len(keys) != 3 {
		t.Fatalf("len(keys) = %d, want 3: %+v", len(keys), keys)
	}
	for _, tc := range []struct {
		label string
		env   string
		key   string
	}{
		{"r_personal", "CURSOR_API_KEY_R_PERSONAL", "crsr-r"},
		{"b_personal", "CURSOR_API_KEY_B_PERSONAL", "crsr-b-personal"},
		{"b_student", "CURSOR_API_KEY_B_STUDENT", "crsr-b-student"},
	} {
		key, ok := findCursorKey(keys, tc.label)
		if !ok {
			t.Errorf("missing label %q in %+v", tc.label, keys)
			continue
		}
		if key.EnvVar != tc.env || key.APIKey != tc.key {
			t.Errorf("label %q = %+v", tc.label, key)
		}
		if key.RefreshToken != "" || key.RefreshTokenEnvVar != "" {
			t.Errorf("label %q unexpectedly has a refresh token: %+v", tc.label, key)
		}
	}
	if _, ok := findCursorKey(keys, "default"); ok {
		t.Error("default label emitted without a base credential")
	}
	if _, ok := findCursorKey(keys, "key"); ok {
		t.Error("bare CURSOR_API_KEY must be ignored")
	}
}

func TestLoadKeysIgnoresBlanksAndLookalikes(t *testing.T) {
	keys := LoadKeys([]string{
		"CURSOR_API_KEY_=",
		"CURSOR_API_KEY_SPACE=   ",
		"CURSOR_API_KEYSTORE=not-a-key",
		"X_CURSOR_API_KEY_B=x",
		"CURSOR_REFRESH_TOKEN=",
		"CURSOR_REFRESH_TOKEN_SPACE=   ",
		"CURSOR_REFRESH_TOKENSTORE=x",
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

func TestLoadKeysLegacyRefreshTokenOnlyBelongsToRPersonal(t *testing.T) {
	keys := LoadKeys([]string{
		"CURSOR_REFRESH_TOKEN=legacy-refresh",
		"CURSOR_API_KEY_B_PERSONAL=crsr-b-personal",
		"CURSOR_REFRESH_TOKEN_B_STUDENT=student-refresh",
	})
	if len(keys) != 3 {
		t.Fatalf("keys = %+v", keys)
	}

	r, ok := findCursorKey(keys, "r_personal")
	if !ok {
		t.Fatal("missing refresh-only r_personal credential")
	}
	if r.APIKey != "" || r.RefreshToken != "legacy-refresh" || r.RefreshTokenEnvVar != "CURSOR_REFRESH_TOKEN" {
		t.Errorf("r_personal = %+v", r)
	}

	b, ok := findCursorKey(keys, "b_personal")
	if !ok {
		t.Fatal("missing b_personal")
	}
	if b.RefreshToken != "" {
		t.Errorf("legacy refresh token leaked to b_personal: %+v", b)
	}

	student, ok := findCursorKey(keys, "b_student")
	if !ok {
		t.Fatal("missing b_student")
	}
	if student.RefreshToken != "student-refresh" || student.RefreshTokenEnvVar != "CURSOR_REFRESH_TOKEN_B_STUDENT" {
		t.Errorf("b_student = %+v", student)
	}
}

func TestLoadKeysRefreshTokenAttachesToMatchingKey(t *testing.T) {
	keys := LoadKeys([]string{
		"CURSOR_API_KEY_R_PERSONAL=crsr-r",
		"CURSOR_REFRESH_TOKEN=legacy-refresh",
		"CURSOR_API_KEY_B_STUDENT=crsr-b-student",
		"CURSOR_REFRESH_TOKEN_B_STUDENT=student-refresh",
	})
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}

	r, ok := findCursorKey(keys, "r_personal")
	if !ok {
		t.Fatal("missing r_personal")
	}
	if r.APIKey != "crsr-r" || r.RefreshToken != "legacy-refresh" {
		t.Errorf("r_personal = %+v", r)
	}

	student, ok := findCursorKey(keys, "b_student")
	if !ok {
		t.Fatal("missing b_student")
	}
	if student.APIKey != "crsr-b-student" || student.RefreshToken != "student-refresh" {
		t.Errorf("b_student = %+v", student)
	}
}

func TestLoadKeysSortsByEnvVarAndSanitizesLabel(t *testing.T) {
	keys := LoadKeys([]string{
		"CURSOR_API_KEY_Z=last",
		"CURSOR_API_KEY_A_B=alpha",
		"CURSOR_API_KEY_Ünicode Secret=value",
	})
	if len(keys) != 3 {
		t.Fatalf("keys = %+v", keys)
	}
	if keys[0].EnvVar != "CURSOR_API_KEY_A_B" ||
		keys[1].EnvVar != "CURSOR_API_KEY_Z" ||
		keys[2].EnvVar != "CURSOR_API_KEY_Ünicode Secret" {
		t.Errorf("order = %q, %q, %q", keys[0].EnvVar, keys[1].EnvVar, keys[2].EnvVar)
	}
	if keys[0].Label != "a_b" {
		t.Errorf("label = %q, want a_b", keys[0].Label)
	}
	if keys[2].Label != "nicode_secret" {
		t.Errorf("sanitized label = %q, want nicode_secret", keys[2].Label)
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

func TestKeyConfigAuthModeLabelsNameTheVariable(t *testing.T) {
	key := KeyConfig{
		EnvVar: "CURSOR_API_KEY_B_PERSONAL",
		Label:  "b_personal",
		APIKey: "crsr_secret_value",
	}
	if got := key.AuthModeLabel(); got != "CURSOR_API_KEY_B_PERSONAL mode=api_key" {
		t.Errorf("AuthModeLabel = %q", got)
	}

	refresh := KeyConfig{
		EnvVar:             "CURSOR_REFRESH_TOKEN_B_STUDENT",
		RefreshTokenEnvVar: "CURSOR_REFRESH_TOKEN_B_STUDENT",
		Label:              "b_student",
		RefreshToken:       "plain-refresh-secret",
	}
	if got := refresh.AuthModeLabel(); got != "CURSOR_REFRESH_TOKEN_B_STUDENT mode=oauth_refresh" {
		t.Errorf("AuthModeLabel = %q", got)
	}
}
