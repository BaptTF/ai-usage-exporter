package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const defaultPeriodBody = `{
	"billingCycleStart":"2026-09-01T00:00:00Z",
	"billingCycleEnd":"2026-10-01T00:00:00Z",
	"planUsage":{
		"totalPercentUsed":42.5,
		"autoPercentUsed":30,
		"apiPercentUsed":12.5,
		"totalSpend":4250,
		"includedSpend":4250,
		"bonusSpend":0,
		"remaining":5750,
		"limit":10000
	},
	"spendLimitUsage":{
		"totalSpend":1500,
		"individualLimit":5000,
		"individualUsed":1500,
		"individualRemaining":3500,
		"limitType":"user"
	}
}`

const defaultPlanBody = `{"planInfo":{"planName":"Pro","includedAmountCents":10000,"price":"$20/mo"}}`

const defaultSandBody = `{
	"usagePercent":18.5,
	"hasNonZeroIncludedLimit":true,
	"nextResetTimestampUtc":"2026-09-19T00:00:00Z"
}`

type cursorServer struct {
	*httptest.Server

	periodCalls   atomic.Int32
	planCalls     atomic.Int32
	sandCalls     atomic.Int32
	sandWebCalls  atomic.Int32
	exchangeCalls atomic.Int32
	oauthCalls    atomic.Int32

	periodBody  string
	planBody    string
	sandBody    string
	sandWebBody string

	// periodHook runs before a GetCurrentPeriodUsage response is written. It
	// lets multi-credential tests prove that providers are collected in
	// parallel.
	periodHook func()

	periodStatus   int
	planStatus     int
	sandStatus     int
	sandWebStatus  int
	exchangeStatus int
	oauthStatus    int

	oauthBody    string
	exchangeBody string

	lastCookie string
	lastAuth   string
}

func newCursorServer(t *testing.T) *cursorServer {
	t.Helper()
	s := &cursorServer{
		periodBody:     defaultPeriodBody,
		planBody:       defaultPlanBody,
		sandBody:       defaultSandBody,
		sandWebBody:    `{"usagePercent":99,"hasNonZeroIncludedLimit":true}`,
		periodStatus:   http.StatusOK,
		planStatus:     http.StatusOK,
		sandStatus:     http.StatusOK,
		sandWebStatus:  http.StatusOK,
		exchangeStatus: http.StatusOK,
		oauthStatus:    http.StatusOK,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "exchange_user_api_key"):
			s.exchangeCalls.Add(1)
			w.WriteHeader(s.exchangeStatus)
			s.writeJSON(w, s.exchangeBody)
		case strings.HasSuffix(r.URL.Path, "/oauth/token"):
			s.oauthCalls.Add(1)
			w.WriteHeader(s.oauthStatus)
			s.writeJSON(w, s.oauthBody)
		case strings.HasSuffix(r.URL.Path, "GetCurrentPeriodUsage"):
			s.periodCalls.Add(1)
			s.lastAuth = r.Header.Get("Authorization")
			if s.periodHook != nil {
				s.periodHook()
			}
			w.WriteHeader(s.periodStatus)
			s.writeJSON(w, s.periodBody)
		case strings.HasSuffix(r.URL.Path, "GetPlanInfo"):
			s.planCalls.Add(1)
			w.WriteHeader(s.planStatus)
			s.writeJSON(w, s.planBody)
		case strings.HasSuffix(r.URL.Path, "get-sand-usage-status"):
			s.sandWebCalls.Add(1)
			s.lastCookie = r.Header.Get("Cookie")
			w.WriteHeader(s.sandWebStatus)
			s.writeJSON(w, s.sandWebBody)
		case strings.HasSuffix(r.URL.Path, "GetSandUsageStatus"):
			s.sandCalls.Add(1)
			w.WriteHeader(s.sandStatus)
			s.writeJSON(w, s.sandBody)
		default:
			http.NotFound(w, r)
		}
	})
	s.Server = httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func (s *cursorServer) writeJSON(w http.ResponseWriter, body string) {
	if body == "" {
		body = "boom"
	}
	_, _ = w.Write([]byte(body))
}

func (s *cursorServer) clientWithAPIKey(t *testing.T, key string) *Client {
	t.Helper()
	return NewClient(Config{
		APIKey:      key,
		Timeout:     time.Second,
		APIBase:     s.URL,
		OAuthURL:    s.URL + "/oauth/token",
		ExchangeURL: s.URL + "/auth/exchange_user_api_key",
		WebBase:     s.URL,
	})
}

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func accessJWT(t *testing.T) string {
	return makeJWT(t, map[string]any{
		"sub": "auth0|user_abc",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
}

func TestFetchStatusOK(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	c := s.clientWithAPIKey(t, "crsr_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if st.PlanName != "Pro" || st.PlanPrice != "$20/mo" {
		t.Errorf("plan = %q %q", st.PlanName, st.PlanPrice)
	}
	if st.Period == nil {
		t.Fatal("Period nil")
	}
	p := st.Period
	if p.TotalPercent != 42.5 || p.AutoPercent != 30 || p.APIPercent != 12.5 {
		t.Errorf("percents = %+v", p)
	}
	if p.SpendCents != 4250 || p.LimitCents != 10000 {
		t.Errorf("spend/limit = %d/%d", p.SpendCents, p.LimitCents)
	}
	if p.OnDemand == nil || p.OnDemand.SpendCents != 1500 || p.OnDemand.LimitCents != 5000 {
		t.Errorf("OnDemand = %+v", p.OnDemand)
	}
	if st.Grok == nil || st.Grok.UsagePercent != 18.5 || st.Grok.ResetsAt == nil {
		t.Errorf("Grok = %+v", st.Grok)
	}
	if st.GrokError != "" || st.PlanError != "" {
		t.Errorf("unexpected soft errors: grok=%q plan=%q", st.GrokError, st.PlanError)
	}
	if !strings.HasPrefix(s.lastAuth, "Bearer ") {
		t.Errorf("Authorization = %q", s.lastAuth)
	}
	if s.exchangeCalls.Load() != 1 || s.periodCalls.Load() != 1 {
		t.Errorf("calls exchange=%d period=%d", s.exchangeCalls.Load(), s.periodCalls.Load())
	}
}

func TestFetchStatusCancellationAcrossRequests(t *testing.T) {
	const (
		blockPeriod   = "period"
		blockPlan     = "plan"
		blockGrok     = "grok"
		blockGrokWeb  = "grok_web"
		blockOAuth    = "oauth"
		blockExchange = "exchange"
	)
	cases := []struct {
		name    string
		block   string
		refresh bool
	}{
		{"api key exchange", blockExchange, false},
		{"period", blockPeriod, false},
		{"plan info", blockPlan, false},
		{"grok rpc", blockGrok, false},
		{"grok web fallback", blockGrokWeb, false},
		{"oauth refresh", blockOAuth, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := accessJWT(t)
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
				case strings.HasSuffix(r.URL.Path, "/oauth/token"):
					if tc.block == blockOAuth {
						block()
						return
					}
					_, _ = w.Write([]byte(`{"access_token":"` + token + `"}`))
				case strings.Contains(r.URL.Path, "exchange_user_api_key"):
					if tc.block == blockExchange {
						block()
						return
					}
					_, _ = w.Write([]byte(`{"accessToken":"` + token + `"}`))
				case strings.HasSuffix(r.URL.Path, "GetCurrentPeriodUsage"):
					if tc.block == blockPeriod {
						block()
						return
					}
					_, _ = w.Write([]byte(defaultPeriodBody))
				case strings.HasSuffix(r.URL.Path, "GetPlanInfo"):
					if tc.block == blockPlan {
						block()
						return
					}
					_, _ = w.Write([]byte(defaultPlanBody))
				case strings.HasSuffix(r.URL.Path, "GetSandUsageStatus"):
					if tc.block == blockGrok {
						block()
						return
					}
					if tc.block == blockGrokWeb {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte("not found"))
						return
					}
					_, _ = w.Write([]byte(defaultSandBody))
				case strings.HasSuffix(r.URL.Path, "get-sand-usage-status"):
					if tc.block == blockGrokWeb {
						block()
						return
					}
					_, _ = w.Write([]byte(`{"usagePercent":99,"hasNonZeroIncludedLimit":true}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			defer close(release)

			cfg := Config{
				Timeout:     30 * time.Second,
				APIBase:     srv.URL,
				OAuthURL:    srv.URL + "/oauth/token",
				ExchangeURL: srv.URL + "/auth/exchange_user_api_key",
				WebBase:     srv.URL,
			}
			if tc.refresh {
				cfg.RefreshToken = "refresh-secret"
			} else {
				cfg.APIKey = "crsr_test"
			}
			c := NewClient(cfg)
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

func TestGrokSoftFailKeepsPeriod(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.sandStatus = http.StatusInternalServerError
	s.sandWebStatus = http.StatusInternalServerError
	c := s.clientWithAPIKey(t, "crsr_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatalf("Grok soft-fail must not fail the whole fetch: %v", err)
	}
	if st.Period == nil || st.Period.TotalPercent != 42.5 {
		t.Errorf("Period = %+v", st.Period)
	}
	if st.Grok != nil {
		t.Errorf("Grok = %+v, want nil", st.Grok)
	}
	if st.GrokError == "" {
		t.Error("GrokError empty, want diagnostic")
	}
}

func TestGrokOmittedWithoutAllowance(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.sandBody = `{"usagePercent":0,"hasNonZeroIncludedLimit":false}`
	c := s.clientWithAPIKey(t, "crsr_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Grok != nil {
		t.Errorf("Grok = %+v, want nil", st.Grok)
	}
	if st.GrokError != "" {
		t.Errorf("GrokError = %q, want empty (omission is not an error)", st.GrokError)
	}
}

func TestGrokWebFallback(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.sandStatus = http.StatusNotFound
	s.sandWebStatus = http.StatusOK
	c := s.clientWithAPIKey(t, "crsr_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Grok == nil || st.Grok.UsagePercent != 99 {
		t.Errorf("Grok = %+v (web fallback)", st.Grok)
	}
	if s.sandWebCalls.Load() < 1 {
		t.Error("expected web fallback call")
	}
	if !strings.Contains(s.lastCookie, "WorkosCursorSessionToken=user_abc%3A%3A") {
		t.Errorf("Cookie = %q", s.lastCookie)
	}
}

func TestRefreshOn401(t *testing.T) {
	oauthBody := `{"access_token":"` + accessJWT(t) + `"}`
	// First period call is rejected; the retry after invalidation succeeds.
	var periodCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth/token"):
			_, _ = w.Write([]byte(oauthBody))
		case strings.HasSuffix(r.URL.Path, "GetCurrentPeriodUsage"):
			if periodCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(defaultPeriodBody))
		case strings.HasSuffix(r.URL.Path, "GetPlanInfo"):
			_, _ = w.Write([]byte(defaultPlanBody))
		case strings.HasSuffix(r.URL.Path, "GetSandUsageStatus"):
			_, _ = w.Write([]byte(defaultSandBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{
		RefreshToken: "refresh-secret",
		Timeout:      time.Second,
		APIBase:      srv.URL,
		OAuthURL:     srv.URL + "/oauth/token",
		WebBase:      srv.URL,
	})
	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Period == nil || periodCalls.Load() != 2 {
		t.Errorf("period calls = %d, period = %+v", periodCalls.Load(), st.Period)
	}
}

func TestShouldLogoutIsAuthErrorAndLeaksNothing(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"","shouldLogout":true}`
	c := s.clientWithAPIKey(t, "crsr_very_secret")

	_, err := c.FetchStatus(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsAuthError(err) {
		t.Errorf("err = %v (%T), want AuthError", err, err)
	}
	if !strings.Contains(err.Error(), "shouldLogout") {
		t.Errorf("err = %q, want shouldLogout mention", err)
	}
	if strings.Contains(err.Error(), "crsr_very_secret") {
		t.Errorf("error leaks the key: %s", err)
	}
}

func TestParseOnDemandTeamPooled(t *testing.T) {
	raw := []byte(`{
		"billingCycleStart":"2026-09-01T00:00:00Z",
		"billingCycleEnd":"2026-10-01T00:00:00Z",
		"planUsage":{"totalPercentUsed":10,"includedSpend":100,"limit":1000,"totalSpend":100},
		"spendLimitUsage":{
			"totalSpend":447,
			"pooledLimit":"2000",
			"pooledUsed":447,
			"pooledRemaining":1553,
			"individualLimit":0,
			"limitType":"team"
		}
	}`)
	p, err := parsePeriodUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.OnDemand == nil {
		t.Fatal("OnDemand nil")
	}
	if p.OnDemand.SpendCents != 447 || p.OnDemand.LimitCents != 2000 {
		t.Errorf("OnDemand = %+v", p.OnDemand)
	}
	if p.OnDemand.LimitType != "team" {
		t.Errorf("LimitType = %q", p.OnDemand.LimitType)
	}
	if p.OnDemand.Percent != 447.0*100/2000 {
		t.Errorf("Percent = %v", p.OnDemand.Percent)
	}
}

func TestParseOnDemandAbsentWhenZero(t *testing.T) {
	raw := []byte(`{
		"planUsage":{"totalPercentUsed":5,"includedSpend":50,"limit":1000,"totalSpend":50},
		"spendLimitUsage":{"totalSpend":0,"individualLimit":0,"pooledLimit":0,"limitType":"user"}
	}`)
	p, err := parsePeriodUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.OnDemand != nil {
		t.Errorf("OnDemand = %+v, want nil when spend and limit are 0", p.OnDemand)
	}
}

func TestParseOnDemandSpendWithoutLimit(t *testing.T) {
	raw := []byte(`{
		"planUsage":{"totalPercentUsed":100,"includedSpend":1000,"limit":1000,"totalSpend":1000},
		"spendLimitUsage":{"totalSpend":250,"individualLimit":0,"limitType":"user"}
	}`)
	p, err := parsePeriodUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.OnDemand == nil || p.OnDemand.SpendCents != 250 {
		t.Fatalf("OnDemand = %+v", p.OnDemand)
	}
	if p.OnDemand.LimitCents != 0 || p.OnDemand.Percent != 0 {
		t.Errorf("limit/percent should be unset: %+v", p.OnDemand)
	}
}

func TestPlanInfoIncludedAmountFallback(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.periodBody = `{
		"billingCycleStart":"2026-09-01T00:00:00Z",
		"billingCycleEnd":"2026-10-01T00:00:00Z",
		"planUsage":{"totalPercentUsed":50,"includedSpend":20000,"totalSpend":20000}
	}`
	s.planBody = `{"planInfo":{"planName":"Ultra","includedAmountCents":40000,"price":"$200/mo"}}`
	c := s.clientWithAPIKey(t, "crsr_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.PlanName != "Ultra" {
		t.Errorf("PlanName = %q", st.PlanName)
	}
	if st.Period.LimitCents != 40000 {
		t.Errorf("LimitCents = %d (want GetPlanInfo fallback)", st.Period.LimitCents)
	}
}

func TestPlanInfoSoftFailMarksPlanError(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.planStatus = http.StatusInternalServerError
	s.planBody = `boom`
	c := s.clientWithAPIKey(t, "crsr_test")

	st, err := c.FetchStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Period == nil {
		t.Fatal("Period must survive GetPlanInfo failure")
	}
	if st.PlanError == "" {
		t.Error("PlanError empty, want diagnostic")
	}
}

func TestParseMonthlyPriceUSD(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"$20/mo", 20, true},
		{"$20 / month", 20, true},
		{"USD 200 per month", 200, true},
		{"20/monthly", 20, true},
		{" $200/MO ", 200, true},
		{"$20/year", 0, false},
		{"$20", 0, false},
		{"free", 0, false},
		{"$0/mo", 0, false},
		{"", 0, false},
		{"EUR 20/mo", 0, false},
	}
	for _, tc := range cases {
		got, ok := ParseMonthlyPriceUSD(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseMonthlyPriceUSD(%q) = %v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestJWTSubStripsProviderPrefix(t *testing.T) {
	token := makeJWT(t, map[string]any{"sub": "auth0|user_123"})
	if got, ok := jwtSub(token); !ok || got != "user_123" {
		t.Errorf("jwtSub = %q,%v", got, ok)
	}
	if _, ok := jwtSub("not-a-jwt"); ok {
		t.Error("jwtSub accepted an invalid token")
	}
}

func TestConfigAuthPriorityAndCrsrSafetyNet(t *testing.T) {
	cfg := Config{
		APIKey:          "crsr_key",
		APIKeyVar:       "CURSOR_API_KEY_R_PERSONAL",
		RefreshToken:    "refresh",
		RefreshTokenVar: "CURSOR_REFRESH_TOKEN",
	}
	mode, secret, varName := cfg.resolveAuth()
	if mode != authModeAPIKey || secret != "crsr_key" || varName != "CURSOR_API_KEY_R_PERSONAL" {
		t.Errorf("priority = %v %q %q", mode, secret, varName)
	}

	cfg = Config{
		RefreshToken:    "crsr_in_refresh",
		RefreshTokenVar: "CURSOR_REFRESH_TOKEN",
	}
	mode, secret, varName = cfg.resolveAuth()
	if mode != authModeAPIKey || secret != "crsr_in_refresh" || varName != "CURSOR_REFRESH_TOKEN" {
		t.Errorf("safety net = %v %q %q", mode, secret, varName)
	}

	cfg = Config{
		RefreshToken:    "plain-refresh",
		RefreshTokenVar: "CURSOR_REFRESH_TOKEN_B_STUDENT",
	}
	mode, _, varName = cfg.resolveAuth()
	if mode != authModeRefresh || varName != "CURSOR_REFRESH_TOKEN_B_STUDENT" {
		t.Errorf("oauth mode = %v %q", mode, varName)
	}

	if (Config{}).Enabled() {
		t.Error("Enabled with empty config")
	}
}

func TestParseTimeCursorBillingCycle(t *testing.T) {
	want := time.Date(2026, 12, 9, 14, 6, 5, 0, time.UTC)
	for _, raw := range []string{
		"1796825165000",            // Cursor's current millisecond string
		"1796825165",               // same instant in seconds
		"2026-12-09T14:06:05Z",     // ISO fallback
		"2026-12-09T14:06:05.000Z", // ISO with milliseconds
	} {
		got, ok := parseTime(raw)
		if !ok || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %s, %v; want %s, true", raw, got, ok, want)
		}
	}
	if _, ok := parseTime("not-a-time"); ok {
		t.Error("parseTime accepted invalid input")
	}
}
