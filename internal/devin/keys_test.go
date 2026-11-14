package devin

import "testing"

func TestLoadKeysSuffixedDevins(t *testing.T) {
	keys := LoadKeys([]string{
		"DEVIN_API_KEY_B=cog_b",
		"DEVIN_API_KEY_R=cog_r",
		"DEVIN_API_KEY=cog_legacy",
		"DEVIN_ORG_ID=org-shared",
		"DEVIN_ORG_ID_R=org-r",
		"DEVIN_API_KEY_BLANK=   ",
	})
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	if keys[0].Label != "b" || keys[0].Token != "cog_b" || keys[0].OrgID != "org-shared" {
		t.Errorf("B = %+v", keys[0])
	}
	if keys[1].Label != "r" || keys[1].Token != "cog_r" || keys[1].OrgID != "org-r" {
		t.Errorf("R = %+v", keys[1])
	}
}

func TestLoadKeysBareDevinsMapsToR(t *testing.T) {
	keys := LoadKeys([]string{
		"DEVIN_API_KEY=cog_legacy",
		"DEVIN_ORG_ID=org-shared",
	})
	if len(keys) != 1 {
		t.Fatalf("keys = %+v", keys)
	}
	if keys[0].Label != "r" || keys[0].Token != "cog_legacy" || keys[0].OrgID != "org-shared" {
		t.Errorf("legacy key = %+v", keys[0])
	}
}

func TestLoadKeysSanitizesAndSorts(t *testing.T) {
	keys := LoadKeys([]string{
		"DEVIN_API_KEY_ZED=cog_z",
		"DEVIN_API_KEY_B_PERSONAL=cog_b",
	})
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	if keys[0].Label != "b_personal" || keys[1].Label != "zed" {
		t.Errorf("labels = %q, %q", keys[0].Label, keys[1].Label)
	}
}
