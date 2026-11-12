package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/budget"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

const defaultSelfBody = `{"principal_type":"pat_user","user_id":"user-1","user_name":"Test","org_id":"org-self"}`

const defaultConsumptionBody = `{
	"total_acus": 12.5,
	"consumption_by_date": [
		{"date": 1788307200, "acus": 10.5, "acus_by_product": {"devin": 8, "cascade": 2, "terminal": 0.5, "review": null}},
		{"date": 1788393600, "acus": 2.0, "acus_by_product": {"devin": 1, "cascade": 1, "terminal": 0, "review": null}}
	]
}`

type devinServer struct {
	*httptest.Server

	selfBody          string
	consumptionBody   string
	seatBody          string
	selfStatus        int
	consumptionStatus int
	seatStatus        int

	lastAuth      string
	lastConsumURL string
	lastSeatBody  string
}

func newDevinServer(t *testing.T) *devinServer {
	t.Helper()
	s := &devinServer{
		selfBody:          defaultSelfBody,
		consumptionBody:   defaultConsumptionBody,
		seatBody:          `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":"BILLING_STRATEGY_QUOTA"},"dailyQuotaRemainingPercent":60,"weeklyQuotaRemainingPercent":20,"dailyQuotaResetAtUnix":"1788480000","weeklyQuotaResetAtUnix":1788900000}}}`,
		selfStatus:        http.StatusOK,
		consumptionStatus: http.StatusOK,
		seatStatus:        http.StatusOK,
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/self":
			s.lastAuth = r.Header.Get("Authorization")
			w.WriteHeader(s.selfStatus)
			_, _ = w.Write([]byte(s.selfBody))
		case strings.HasPrefix(r.URL.Path, "/v3/organizations/"):
			s.lastConsumURL = r.URL.String()
			w.WriteHeader(s.consumptionStatus)
			_, _ = w.Write([]byte(s.consumptionBody))
		case strings.HasSuffix(r.URL.Path, "GetUserStatus"):
			var body struct {
				Metadata struct {
					APIKey string `json:"apiKey"`
				} `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.lastSeatBody = body.Metadata.APIKey
			w.WriteHeader(s.seatStatus)
			_, _ = w.Write([]byte(s.seatBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *devinServer) client(token string) *Client {
	return NewClient(Config{
		Token:    token,
		Timeout:  time.Second,
		BaseURL:  s.URL,
		SeatURL:  s.URL,
		ResetDay: 5,
	})
}

func TestFetchStatusOK(t *testing.T) {
	s := newDevinServer(t)
	c := s.client("cog_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OrgID != "org-self" {
		t.Errorf("OrgID = %q", st.OrgID)
	}
	if st.ACUConsumed != 12.5 || st.DayCount != 2 {
		t.Errorf("ACU = %v days=%d", st.ACUConsumed, st.DayCount)
	}
	if st.CycleStart == nil || st.CycleEnd == nil {
		t.Error("cycle bounds missing")
	}
	if st.Quota == nil {
		t.Fatal("Quota nil")
	}
	if st.Quota.DailyUsedPercent == nil || *st.Quota.DailyUsedPercent != 40 {
		t.Errorf("DailyUsedPercent = %v", st.Quota.DailyUsedPercent)
	}
	if st.Quota.WeeklyUsedPercent == nil || *st.Quota.WeeklyUsedPercent != 80 {
		t.Errorf("WeeklyUsedPercent = %v", st.Quota.WeeklyUsedPercent)
	}
	if st.Quota.PlanName != "Pro" {
		t.Errorf("PlanName = %q", st.Quota.PlanName)
	}
	if s.lastAuth != "Bearer cog_test" {
		t.Errorf("Authorization = %q", s.lastAuth)
	}
	if s.lastSeatBody != "cog_test" {
		t.Errorf("seat apiKey = %q", s.lastSeatBody)
	}
	if s.lastConsumURL == "" {
		t.Error("consumption URL never called")
	}
}

func TestFetchStatusCancellationAcrossRequests(t *testing.T) {
	const seatBody = `{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"dailyQuotaRemainingPercent":60,"dailyQuotaResetAtUnix":"1788480000"}}}`
	for _, blockAt := range []string{"self", "quota", "consumption"} {
		t.Run(blockAt, func(t *testing.T) {
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			block := func() {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v3/self":
					if blockAt == "self" {
						block()
						return
					}
					_, _ = w.Write([]byte(defaultSelfBody))
				case strings.HasSuffix(r.URL.Path, "GetUserStatus"):
					if blockAt == "quota" {
						block()
						return
					}
					_, _ = w.Write([]byte(seatBody))
				case strings.HasPrefix(r.URL.Path, "/v3/organizations/"):
					if blockAt == "consumption" {
						block()
						return
					}
					_, _ = w.Write([]byte(defaultConsumptionBody))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			defer close(release)

			c := NewClient(Config{
				Token:    "cog_test",
				Timeout:  30 * time.Second,
				BaseURL:  srv.URL,
				SeatURL:  srv.URL,
				ResetDay: 5,
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				_, err := c.FetchStatus(ctx)
				done <- err
			}()

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("blocked request never reached the server")
			}
			cancel()

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("err = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("FetchStatus did not return after cancellation")
			}
		})
	}
}

func TestFetchStatusTrimsToken(t *testing.T) {
	s := newDevinServer(t)
	c := s.client("  cog_test\n")
	if _, err := c.FetchStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.lastAuth != "Bearer cog_test" {
		t.Errorf("Authorization = %q", s.lastAuth)
	}
}

func TestFetchStatusQuotaSoftFailKeepsACU(t *testing.T) {
	s := newDevinServer(t)
	s.seatStatus = http.StatusInternalServerError
	s.seatBody = `boom`
	c := s.client("cog_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatalf("quota soft-fail must not fail the fetch: %v", err)
	}
	if st.ACUConsumed != 12.5 || st.DayCount != 2 {
		t.Errorf("ACU data lost: %+v", st)
	}
	if st.Quota != nil {
		t.Errorf("Quota = %+v, want nil", st.Quota)
	}
	if st.QuotaError == "" {
		t.Error("QuotaError empty, want diagnostic")
	}
}

func TestFetchStatusOrgFromConfigWins(t *testing.T) {
	s := newDevinServer(t)
	c := s.client("cog_test")
	c.orgID = "org-config"

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.OrgID != "org-config" {
		t.Errorf("OrgID = %q", st.OrgID)
	}
	if !strings.Contains(s.lastConsumURL, "/v3/organizations/org-config/") {
		t.Errorf("consumption URL = %q", s.lastConsumURL)
	}
}

func TestFetchStatusMissingOrgNamesVariable(t *testing.T) {
	s := newDevinServer(t)
	s.selfBody = `{"principal_type":"pat_user"}`
	c := s.client("cog_test")

	_, err := c.FetchStatus(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "DEVIN_ORG_ID") || !strings.Contains(err.Error(), "pat_user") {
		t.Errorf("error = %q", err)
	}
}

func TestFetchStatusOrgInaccessibleAccusesOrg(t *testing.T) {
	s := newDevinServer(t)
	s.consumptionStatus = http.StatusForbidden
	c := s.client("cog_secret")

	_, err := c.FetchStatus(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "org-self") || !strings.Contains(msg, "HTTP 403") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "DEVIN_API_KEY") {
		t.Errorf("org error accuses the key: %q", msg)
	}
	if strings.Contains(msg, "cog_secret") {
		t.Errorf("error leaks the token: %q", msg)
	}
}

func TestFetchStatusTokenRejectedNoLeak(t *testing.T) {
	s := newDevinServer(t)
	s.selfStatus = http.StatusUnauthorized
	c := s.client("cog_very_secret")

	_, err := c.FetchStatus(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "DEVIN_API_KEY") || !strings.Contains(msg, "HTTP 401") {
		t.Errorf("error = %q", msg)
	}
	if strings.Contains(msg, "cog_very_secret") || strings.Contains(msg, "sha256=") || strings.Contains(msg, "len=") {
		t.Errorf("error leaks credential metadata: %q", msg)
	}
}

func TestFetchStatusSendsCycleWindow(t *testing.T) {
	s := newDevinServer(t)
	c := s.client("cog_test")
	c.now = func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	c.resetDay = 5

	if _, err := c.FetchStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(s.lastConsumURL)
	if err != nil {
		t.Fatal(err)
	}
	start, end := CycleBounds(c.now(), 5)
	if got := u.Query().Get("time_after"); got != fmt.Sprint(start.Unix()) {
		t.Errorf("time_after = %q, want %d", got, start.Unix())
	}
	if got := u.Query().Get("time_before"); got != fmt.Sprint(c.now().Unix()) {
		t.Errorf("time_before = %q, want capped now %d", got, c.now().Unix())
	}
	_ = end
}

func TestCycleBounds(t *testing.T) {
	cases := []struct {
		name      string
		now       time.Time
		day       int
		wantStart string
		wantEnd   string
	}{
		{
			name:      "before reset day",
			now:       time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC),
			day:       5,
			wantStart: "2026-09-05T08:00:00Z",
			wantEnd:   "2026-10-05T08:00:00Z",
		},
		{
			name:      "after reset instant",
			now:       time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
			day:       5,
			wantStart: "2026-10-05T08:00:00Z",
			wantEnd:   "2026-11-05T08:00:00Z",
		},
		{
			name:      "exactly at reset instant",
			now:       time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC),
			day:       5,
			wantStart: "2026-10-05T08:00:00Z",
			wantEnd:   "2026-11-05T08:00:00Z",
		},
		{
			name:      "out of range falls back to five",
			now:       time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
			day:       31,
			wantStart: "2026-10-05T08:00:00Z",
			wantEnd:   "2026-11-05T08:00:00Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := CycleBounds(tc.now, tc.day)
			if got := start.Format(time.RFC3339); got != tc.wantStart {
				t.Errorf("start = %s, want %s", got, tc.wantStart)
			}
			if got := end.Format(time.RFC3339); got != tc.wantEnd {
				t.Errorf("end = %s, want %s", got, tc.wantEnd)
			}
		})
	}
}

func TestPlanCycle(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	end := now.Add(24 * time.Hour)
	q := &Quota{PlanStart: &start, PlanEnd: &end}
	if s, e, ok := planCycle(q, now); !ok || !s.Equal(start) || !e.Equal(end) {
		t.Errorf("planCycle = %v %v %v", s, e, ok)
	}

	staleStart := now.Add(-48 * time.Hour)
	staleEnd := now.Add(-24 * time.Hour)
	if _, _, ok := planCycle(&Quota{PlanStart: &staleStart, PlanEnd: &staleEnd}, now); ok {
		t.Error("planCycle accepted a stale cycle")
	}
}

func TestParseQuotaDailyOnly(t *testing.T) {
	q, err := parseQuota([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"dailyQuotaRemainingPercent":75,"dailyQuotaResetAtUnix":"1788480000"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.DailyUsedPercent == nil || *q.DailyUsedPercent != 25 {
		t.Errorf("daily = %v", q.DailyUsedPercent)
	}
	if q.WeeklyUsedPercent != nil {
		t.Errorf("weekly = %v, want nil", q.WeeklyUsedPercent)
	}
	if q.DailyResetsAt == nil {
		t.Error("daily reset nil")
	}
}

func TestParseQuotaHideDaily(t *testing.T) {
	q, err := parseQuota([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","hideDailyQuota":true},"dailyQuotaRemainingPercent":10,"weeklyQuotaRemainingPercent":50}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.DailyUsedPercent != nil {
		t.Errorf("daily = %v, want nil when hidden", q.DailyUsedPercent)
	}
	if q.WeeklyUsedPercent == nil || *q.WeeklyUsedPercent != 50 {
		t.Errorf("weekly = %v", q.WeeklyUsedPercent)
	}
}

func TestParseQuotaWeeklyExhaustedProtoOmitted(t *testing.T) {
	// Devin Max hides the daily quota. An exhausted weekly window arrives with
	// weeklyQuotaResetAtUnix but no weeklyQuotaRemainingPercent: protobuf JSON
	// omits the zero. It must surface as 100% used, not as a missing quota.
	q, err := parseQuota([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Max","hideDailyQuota":true},"dailyQuotaRemainingPercent":100,"dailyQuotaResetAtUnix":"1794556800","weeklyQuotaResetAtUnix":"1794729600"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.DailyUsedPercent != nil {
		t.Errorf("daily = %v, want nil when hidden", q.DailyUsedPercent)
	}
	if q.WeeklyUsedPercent == nil || *q.WeeklyUsedPercent != 100 {
		t.Errorf("weekly = %v, want 100", q.WeeklyUsedPercent)
	}
	if q.WeeklyResetsAt == nil {
		t.Error("weekly reset nil")
	}
}

func TestParseQuotaDailyExhaustedProtoOmitted(t *testing.T) {
	q, err := parseQuota([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"},"dailyQuotaResetAtUnix":"1794556800"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.DailyUsedPercent == nil || *q.DailyUsedPercent != 100 {
		t.Errorf("daily = %v, want 100", q.DailyUsedPercent)
	}
	if q.WeeklyUsedPercent != nil {
		t.Errorf("weekly = %v, want nil without reset", q.WeeklyUsedPercent)
	}
}

func TestParseQuotaMissingPercentWithoutReset(t *testing.T) {
	q, err := parseQuota([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.DailyUsedPercent != nil || q.WeeklyUsedPercent != nil {
		t.Errorf("invented quotas: daily=%v weekly=%v", q.DailyUsedPercent, q.WeeklyUsedPercent)
	}
}

func TestParseQuotaOnDemandAndBilling(t *testing.T) {
	q, err := parseQuota([]byte(`{"userStatus":{"planStatus":{"planInfo":{"planName":"Pro","billingStrategy":"BILLING_STRATEGY_QUOTA"},"dailyQuotaRemainingPercent":50,"overageBalanceMicros":"623376568"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if q.OnDemandBalanceUSD == nil || *q.OnDemandBalanceUSD != 623.376568 {
		t.Errorf("OnDemandBalanceUSD = %v", q.OnDemandBalanceUSD)
	}
	if q.BillingStrategy != "BILLING_STRATEGY_QUOTA" {
		t.Errorf("BillingStrategy = %q", q.BillingStrategy)
	}
}

func TestParseQuotaEmptyPlanStatus(t *testing.T) {
	if _, err := parseQuota([]byte(`{"userStatus":{"planStatus":{}}}`)); err == nil {
		t.Fatal("expected error")
	}
}

func TestMicroUSDAbsentOrInvalid(t *testing.T) {
	if _, ok := microsUSD(""); ok {
		t.Error("empty micros accepted")
	}
	if _, ok := microsUSD("not-a-number"); ok {
		t.Error("invalid micros accepted")
	}
}

func TestResetDayFromValue(t *testing.T) {
	cases := map[string]int{
		"":    5,
		"1":   1,
		"28":  28,
		"29":  5,
		"0":   5,
		"abc": 5,
	}
	for in, want := range cases {
		if got := ResetDayFromValue(in); got != want {
			t.Errorf("ResetDayFromValue(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("DEVIN_API_KEY", " cog_test\n")
	t.Setenv("DEVIN_ORG_ID", " org-123 ")
	t.Setenv("DEVIN_RESET_DAY", "12")
	cfg := ConfigFromEnv(time.Second)
	if cfg.Token != "cog_test" || cfg.OrgID != "org-123" || cfg.ResetDay != 12 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestProviderCollectPartialKeepsACU(t *testing.T) {
	s := newDevinServer(t)
	s.seatStatus = http.StatusInternalServerError
	p := NewProvider(s.client("cog_test"), "default")

	res := p.Collect(context.Background())[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Partial {
		t.Error("Partial = false, want true when seat quotas fail")
	}
	if !strings.Contains(res.Diagnostic, "quota:") || !strings.Contains(res.Diagnostic, "HTTP 500") {
		t.Errorf("Diagnostic = %q, want the seat quota error", res.Diagnostic)
	}
	if res.Data.ACU == nil || res.Data.ACU.Consumed != 12.5 {
		t.Errorf("ACU data lost: %+v", res.Data.ACU)
	}
	if len(res.Data.Quotas) != 0 {
		t.Errorf("quotas = %+v, want none", res.Data.Quotas)
	}
}

func TestProviderBuildACUAndQuotas(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	resetDaily := now.Add(12 * time.Hour)
	resetWeekly := now.Add(3 * 24 * time.Hour)
	st := Status{
		ACUConsumed: 12.5,
		Days: []DayUsage{
			{ACUsByProduct: ACUsByProduct{Devin: 8, Cascade: 2, Terminal: 0.5}},
			{ACUsByProduct: ACUsByProduct{Devin: 1, Cascade: 1, Terminal: 0}},
		},
		Quota: &Quota{
			DailyUsedPercent:  floatPtr(40),
			DailyResetsAt:     &resetDaily,
			WeeklyUsedPercent: floatPtr(80),
			WeeklyResetsAt:    &resetWeekly,
		},
	}
	acu := BuildACU(st)
	if acu == nil || acu.Consumed != 12.5 {
		t.Fatalf("ACU = %+v", acu)
	}
	if acu.ByProduct["devin"] != 9 || acu.ByProduct["cascade"] != 3 || acu.ByProduct["terminal"] != 0.5 {
		t.Errorf("products = %+v", acu.ByProduct)
	}
	if acu.Limit != nil {
		t.Error("ACU limit invented")
	}

	quotas := BuildQuotas(st, now)
	if len(quotas) != 2 {
		t.Fatalf("quotas = %+v", quotas)
	}
	if quotas[0].Window != "daily" || quotas[0].UsedRatio != 0.4 {
		t.Errorf("daily quota = %+v", quotas[0])
	}
	if quotas[1].Window != "weekly" || quotas[1].UsedRatio != 0.8 {
		t.Errorf("weekly quota = %+v", quotas[1])
	}
	if quotas[0].Severity == nil || quotas[1].Severity == nil {
		t.Error("severity missing")
	}
}

func TestBuildQuotasSeverityUsesAbsoluteLevel(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	reset := now.Add(23 * time.Hour)
	q := newQuota(snapshot.WindowDaily, 40, &reset, budget.KindDaily, now)
	if q.DryDays == nil || *q.DryDays <= 0 {
		t.Fatalf("test setup should produce a pace warning: %+v", q)
	}
	if q.Severity == nil || *q.Severity != 0 {
		t.Errorf("severity = %v, want absolute green despite pace", q.Severity)
	}

	withoutReset := newQuota(snapshot.WindowWeekly, 95, nil, budget.KindWeekly, now)
	if withoutReset.Severity == nil || *withoutReset.Severity != 2 {
		t.Errorf("severity without reset = %v, want absolute red", withoutReset.Severity)
	}
}

func TestBuildACUWithoutDaysOnlyAll(t *testing.T) {
	acu := BuildACU(Status{ACUConsumed: 3})
	if acu.ByProduct != nil {
		t.Errorf("ByProduct = %+v, want nil when no daily breakdown", acu.ByProduct)
	}
}

func floatPtr(v float64) *float64 { return &v }
