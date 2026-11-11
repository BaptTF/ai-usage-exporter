package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const usageBody = `{
  "usage": {
    "rolling": {"status":"ok","percent":12,"resetsAt":"2026-09-19T18:00:00.123Z"},
    "weekly":  {"status":"ok","percent":38,"resetsAt":"2026-08-31T00:00:00.224Z"},
    "monthly": {"status":"ok","percent":28,"resetsAt":"2026-09-22T01:23:30.224Z"}
  }
}`

func TestFetchUsageAndParseWindows(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(usageBody))
	}))
	defer srv.Close()

	c := NewClientWithURL(time.Second, srv.URL)
	windows, err := c.FetchUsage(context.Background(), "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "ai-usage-exporter/") {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if len(windows) != 3 {
		t.Fatalf("windows = %+v", windows)
	}
	if windows[0].Kind != KindMonthly || windows[0].Percent != 28 {
		t.Errorf("monthly = %+v", windows[0])
	}
	if windows[1].Kind != KindWeekly || windows[1].Percent != 38 {
		t.Errorf("weekly = %+v", windows[1])
	}
	if windows[2].Kind != KindRolling || windows[2].Percent != 12 {
		t.Errorf("rolling = %+v", windows[2])
	}
	if windows[0].ResetsAt.IsZero() {
		t.Error("monthly ResetsAt is zero")
	}
}

func TestFetchUsageErrorsDoNotLeakTheKey(t *testing.T) {
	const secret = "super-secret-api-key"
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":"bad key"}`},
		{"forbidden", http.StatusForbidden, "forbidden"},
		{"rate limited", http.StatusTooManyRequests, "slow down"},
		{"server error", http.StatusInternalServerError, "boom"},
		{"empty windows", http.StatusOK, `{"usage":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClientWithURL(time.Second, srv.URL)
			_, err := c.FetchUsage(context.Background(), secret)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks the key: %s", err)
			}
		})
	}
}

func TestFetchUsageCancellationAbortsBlockedServer(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewClientWithURL(30*time.Second, srv.URL).FetchUsage(ctx, "secret-key")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the server")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchUsage did not return after cancellation")
	}
}

func TestParseResponseMalformed(t *testing.T) {
	if _, err := parseResponse([]byte(`{`)); err == nil {
		t.Fatal("expected JSON error")
	}
}

func TestBuildQuotas(t *testing.T) {
	now := mustTime("2026-08-25T22:47:54.286Z")
	windows := []Window{
		{Name: "Monthly", Kind: KindMonthly, Percent: 28, ResetsAt: mustTime("2026-09-22T01:23:30.224Z")},
		{Name: "Weekly", Kind: KindWeekly, Percent: 38, ResetsAt: mustTime("2026-08-31T00:00:00.224Z")},
		{Name: "Rolling 5h", Kind: KindRolling, Percent: 91, ResetsAt: now.Add(2 * time.Hour)},
	}
	quotas := BuildQuotas(windows, now)
	if len(quotas) != 3 {
		t.Fatalf("quotas = %+v", quotas)
	}

	monthly := quotas[0]
	if monthly.Window != "monthly" || monthly.Quota != "total" || monthly.UsedRatio != 0.28 {
		t.Errorf("monthly = %+v", monthly)
	}
	if monthly.ElapsedRatio == nil || monthly.DryDays == nil || monthly.Severity == nil {
		t.Fatalf("monthly derived fields missing: %+v", monthly)
	}
	if *monthly.Severity != 2 {
		t.Errorf("monthly severity = %d, want red (2)", *monthly.Severity)
	}

	rolling := quotas[2]
	if rolling.Window != "rolling_5h" {
		t.Errorf("rolling window = %q", rolling.Window)
	}
	if rolling.ElapsedRatio != nil || rolling.DryDays != nil {
		t.Errorf("rolling must not expose elapsed/dry: %+v", rolling)
	}
	if rolling.Severity == nil || *rolling.Severity != 2 {
		t.Errorf("rolling severity = %v, want AbsoluteLevel red (2)", rolling.Severity)
	}
}

func TestBuildQuotasFallsBackToAbsoluteLevelWithoutReset(t *testing.T) {
	quotas := BuildQuotas([]Window{
		{Kind: KindMonthly, Percent: 100},
		{Kind: KindWeekly, Percent: 90},
	}, time.Now())
	if len(quotas) != 2 {
		t.Fatalf("quotas = %+v", quotas)
	}
	for _, q := range quotas {
		if q.Severity == nil || *q.Severity != 2 {
			t.Errorf("%s severity = %v, want AbsoluteLevel red (2)", q.Window, q.Severity)
		}
		if q.ElapsedRatio != nil || q.DryDays != nil {
			t.Errorf("%s must not invent elapsed/dry without a reset: %+v", q.Window, q)
		}
		if !q.ResetAt.IsZero() {
			t.Errorf("%s reset = %s, want zero", q.Window, q.ResetAt)
		}
	}
}

func TestBuildQuotasClampsPercent(t *testing.T) {
	quotas := BuildQuotas([]Window{
		{Kind: KindRolling, Percent: -5},
		{Kind: KindRolling, Percent: 150},
	}, time.Now())
	if quotas[0].UsedRatio != 0 {
		t.Errorf("negative percent = %v", quotas[0].UsedRatio)
	}
	if quotas[1].UsedRatio != 1 {
		t.Errorf("oversized percent = %v", quotas[1].UsedRatio)
	}
}
