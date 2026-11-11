// Package cursor reads Cursor's dashboard usage and optional Grok Bot
// allowance. The endpoints are undocumented but are the ones the desktop app
// uses; the exporter isolates all of that here.
package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Cursor APIs — undocumented (OpenUsage / OpenQuota / AI-Usage).
//
//	POST …/aiserver.v1.DashboardService/GetCurrentPeriodUsage
//	POST …/aiserver.v1.DashboardService/GetPlanInfo            (optional)
//	POST …/aiserver.v1.DashboardService/GetSandUsageStatus     (Grok Bot)
//
// Auth: Bearer <access_token> + Connect-Protocol-Version: 1, body {}.
// Grok Bot fallback: POST https://cursor.com/api/dashboard/get-sand-usage-status
// with cookie WorkosCursorSessionToken=<sub>%3A%3A<access_token>.
//
// Grok Bot is a soft failure: Cursor monthly metrics are still returned.

const (
	defaultAPIBase = "https://api2.cursor.sh"
	defaultWebBase = "https://cursor.com"

	periodUsagePath = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	planInfoPath    = "/aiserver.v1.DashboardService/GetPlanInfo"
	sandUsagePath   = "/aiserver.v1.DashboardService/GetSandUsageStatus"
	sandWebPath     = "/api/dashboard/get-sand-usage-status"
)

// Status holds Cursor primary usage plus optional Grok Bot allowance.
type Status struct {
	PlanName string `json:"planName,omitempty"`
	// PlanPrice is the raw price string from GetPlanInfo (for example
	// "$20/mo"). It is never invented and may be empty.
	PlanPrice string `json:"planPrice,omitempty"`

	// Period is monthly usage. Nil only when the primary fetch failed.
	Period *PeriodUsage `json:"period,omitempty"`

	// Grok is the weekly allowance. Nil when absent or soft-failed.
	Grok *GrokUsage `json:"grok,omitempty"`

	// GrokError is a soft diagnostic; it does not hide Period.
	GrokError string `json:"grokError,omitempty"`
	// PlanError is a soft diagnostic when GetPlanInfo is unavailable.
	PlanError string `json:"planError,omitempty"`

	FetchedAt time.Time `json:"fetchedAt"`
	Error     string    `json:"error,omitempty"`
}

// PeriodUsage is the Cursor monthly quota (GetCurrentPeriodUsage).
// Amounts are in USD cents (Cursor billing).
type PeriodUsage struct {
	TotalPercent float64 `json:"totalPercent"`
	AutoPercent  float64 `json:"autoPercent"`
	APIPercent   float64 `json:"apiPercent"`

	// Included plan allowance — cents USD.
	// SpendCents = planUsage.totalSpend (= includedSpend + bonusSpend).
	// LimitCents = planUsage.limit.
	SpendCents         int64 `json:"spendCents"`
	IncludedSpendCents int64 `json:"includedSpendCents"`
	BonusSpendCents    int64 `json:"bonusSpendCents,omitempty"`
	LimitCents         int64 `json:"limitCents"`
	RemainingCents     int64 `json:"remainingCents,omitempty"`

	// Out-of-plan / on-demand (spendLimitUsage). Nil when absent or all zero.
	OnDemand *OnDemandUsage `json:"onDemand,omitempty"`

	CycleStart *time.Time `json:"cycleStart,omitempty"`
	CycleEnd   *time.Time `json:"cycleEnd,omitempty"`
}

// OnDemandUsage is the current period's usage-based budget.
type OnDemandUsage struct {
	SpendCents     int64   `json:"spendCents"`
	LimitCents     int64   `json:"limitCents,omitempty"` // 0 = unknown limit
	RemainingCents int64   `json:"remainingCents,omitempty"`
	Percent        float64 `json:"percent,omitempty"`   // spend/limit*100 when limit > 0
	LimitType      string  `json:"limitType,omitempty"` // "user" | "team"
}

// GrokUsage is the Grok Bot weekly allowance (GetSandUsageStatus).
type GrokUsage struct {
	UsagePercent float64    `json:"usagePercent"`
	ResetsAt     *time.Time `json:"resetsAt,omitempty"`
}

// Config is what the client needs to authenticate and reach Cursor.
type Config struct {
	// APIKey: CURSOR_API_KEY (canonical, crsr_… prefix). Takes priority.
	APIKey string
	// RefreshToken: CURSOR_REFRESH_TOKEN (OAuth). A crsr_ prefix is treated as
	// an API key as a safety net for a renamed secret.
	RefreshToken string
	Timeout      time.Duration
	APIBase      string
	OAuthURL     string
	ExchangeURL  string
	WebBase      string
}

// ConfigFromEnv reads Cursor configuration. The provider is disabled when
// neither credential is set.
func ConfigFromEnv(timeout time.Duration) Config {
	return Config{
		APIKey:       APIKeyFromEnv(),
		RefreshToken: RefreshTokenFromEnv(),
		Timeout:      timeout,
	}
}

// APIKeyFromEnv reads CURSOR_API_KEY, trimming trailing whitespace. Kubernetes
// and secret stores commonly add a final newline.
func APIKeyFromEnv() string {
	return strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))
}

// RefreshTokenFromEnv reads CURSOR_REFRESH_TOKEN, trimming whitespace.
func RefreshTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv("CURSOR_REFRESH_TOKEN"))
}

// Enabled reports whether a Cursor credential is configured.
func (c Config) Enabled() bool {
	return c.APIKey != "" || c.RefreshToken != ""
}

// resolveAuth chooses the mode. CURSOR_API_KEY wins; a refresh prefixed crsr_
// is treated as an API key.
func (c Config) resolveAuth() (mode authMode, secret, secretVar string) {
	if c.APIKey != "" {
		return authModeAPIKey, c.APIKey, "CURSOR_API_KEY"
	}
	if c.RefreshToken == "" {
		return authModeNone, "", ""
	}
	if strings.HasPrefix(c.RefreshToken, apiKeyPrefix) {
		return authModeAPIKey, c.RefreshToken, "CURSOR_REFRESH_TOKEN"
	}
	return authModeRefresh, c.RefreshToken, "CURSOR_REFRESH_TOKEN"
}

// PresenceSecret returns a non-empty credential for enabling the provider.
// The value is a sentinel only and is never logged.
func (c Config) PresenceSecret() string {
	_, secret, _ := c.resolveAuth()
	return secret
}

// AuthModeLabel describes the mode for startup logs (never the secret).
func (c Config) AuthModeLabel() string {
	mode, _, secretVar := c.resolveAuth()
	if mode == authModeNone {
		return "none"
	}
	return secretVar + " mode=" + mode.String()
}

// Client calls Cursor's DashboardService APIs with renewable auth.
type Client struct {
	http *http.Client
	auth *authenticator
	api  string
	web  string
	now  func() time.Time
}

// NewClient creates a Cursor client.
func NewClient(cfg Config) *Client {
	api := cfg.APIBase
	if api == "" {
		api = defaultAPIBase
	}
	web := cfg.WebBase
	if web == "" {
		web = defaultWebBase
	}
	httpClient := &http.Client{Timeout: cfg.Timeout}
	return &Client{
		http: httpClient,
		auth: newAuthenticator(httpClient, cfg),
		api:  strings.TrimRight(api, "/"),
		web:  strings.TrimRight(web, "/"),
		now:  time.Now,
	}
}

// FetchStatus fetches monthly Cursor usage, then Grok Bot as a soft failure.
func (c *Client) FetchStatus(ctx context.Context) (Status, error) {
	period, plan, planErr, err := c.fetchPeriod(ctx)
	if err != nil {
		return Status{}, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Status{}, ctxErr
	}
	st := Status{
		PlanName:  plan.PlanName,
		PlanPrice: plan.Price,
		Period:    &period,
		FetchedAt: c.now(),
	}
	if planErr != nil {
		st.PlanError = planErr.Error()
	}

	grok, gerr := c.fetchGrok(ctx)
	if gerr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Status{}, ctxErr
		}
		st.GrokError = gerr.Error()
	} else if grok != nil {
		st.Grok = grok
	}
	return st, nil
}

func (c *Client) fetchPeriod(ctx context.Context) (PeriodUsage, planInfoParsed, error, error) {
	raw, err := c.connectRPC(ctx, periodUsagePath, true)
	if err != nil {
		return PeriodUsage{}, planInfoParsed{}, nil, err
	}
	period, err := parsePeriodUsage(raw)
	if err != nil {
		return PeriodUsage{}, planInfoParsed{}, nil, err
	}

	var plan planInfoParsed
	var planErr error
	planRaw, perr := c.connectRPC(ctx, planInfoPath, false)
	if perr != nil {
		planErr = perr
	} else {
		plan = parsePlanInfo(planRaw)
		// Fallback: includedAmountCents from GetPlanInfo when planUsage.limit
		// is absent.
		if period.LimitCents == 0 && plan.IncludedAmountCents > 0 {
			period.LimitCents = plan.IncludedAmountCents
		}
	}
	return period, plan, planErr, nil
}

func (c *Client) fetchGrok(ctx context.Context) (*GrokUsage, error) {
	raw, err := c.connectRPC(ctx, sandUsagePath, true)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		raw, err = c.fetchGrokWeb(ctx)
		if err != nil {
			return nil, err
		}
	}
	return parseGrokUsage(raw)
}

// connectRPC POSTs body {} with Bearer + Connect-Protocol-Version: 1.
// retryOn401 re-authenticates once after an expired access token.
func (c *Client) connectRPC(ctx context.Context, path string, retryOn401 bool) ([]byte, error) {
	token, err := c.auth.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.api+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ai-usage-exporter/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error (%s): %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error (%s): %w", path, err)
	}

	if resp.StatusCode == http.StatusUnauthorized && retryOn401 {
		c.auth.invalidateAccess()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return c.connectRPC(ctx, path, false)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		hint := "renew " + c.auth.secretVar
		if c.auth.mode == authModeAPIKey {
			hint = "check " + c.auth.secretVar + " (crsr_… API key)"
		}
		return nil, &AuthError{
			Msg: fmt.Sprintf("Cursor API rejected (HTTP %d): session expired — %s", resp.StatusCode, hint),
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// fetchGrokWeb is the WorkosCursorSessionToken cookie fallback on cursor.com.
func (c *Client) fetchGrokWeb(ctx context.Context) ([]byte, error) {
	token, err := c.auth.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	sub, ok := jwtSub(token)
	if !ok {
		return nil, fmt.Errorf("unreadable JWT subject for Grok Bot cookie")
	}
	// OpenQuota: userId%3A%3Aaccess_token (only "::" is encoded).
	cookie := "WorkosCursorSessionToken=" + sub + "%3A%3A" + token

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.web+sandWebPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.web)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "ai-usage-exporter/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error (get-sand-usage-status): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error (get-sand-usage-status): %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get-sand-usage-status HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// ---- parsing ----------------------------------------------------------------

type periodUsageWire struct {
	BillingCycleStart string          `json:"billingCycleStart"`
	BillingCycleEnd   string          `json:"billingCycleEnd"`
	PlanUsage         *planUsageWire  `json:"planUsage"`
	PlanUsageAlt      *planUsageWire  `json:"plan_usage"`
	SpendLimitUsage   *spendLimitWire `json:"spendLimitUsage"`
	SpendLimitAlt     *spendLimitWire `json:"spend_limit_usage"`
}

type planUsageWire struct {
	TotalPercentUsed json.Number `json:"totalPercentUsed"`
	AutoPercentUsed  json.Number `json:"autoPercentUsed"`
	APIPercentUsed   json.Number `json:"apiPercentUsed"`
	TotalSpend       json.Number `json:"totalSpend"`
	IncludedSpend    json.Number `json:"includedSpend"`
	BonusSpend       json.Number `json:"bonusSpend"`
	Remaining        json.Number `json:"remaining"`
	Limit            json.Number `json:"limit"`
	TotalPercent     json.Number `json:"totalPercent"`
	AutoPercent      json.Number `json:"autoPercent"`
	APIPercent       json.Number `json:"apiPercent"`
	Spend            json.Number `json:"spend"`
}

// spendLimitWire = out-of-plan / on-demand.
type spendLimitWire struct {
	TotalSpend          json.Number `json:"totalSpend"`
	PooledLimit         json.Number `json:"pooledLimit"`
	PooledUsed          json.Number `json:"pooledUsed"`
	PooledRemaining     json.Number `json:"pooledRemaining"`
	IndividualLimit     json.Number `json:"individualLimit"`
	IndividualUsed      json.Number `json:"individualUsed"`
	IndividualRemaining json.Number `json:"individualRemaining"`
	LimitType           string      `json:"limitType"`
}

func parsePeriodUsage(raw []byte) (PeriodUsage, error) {
	var wire periodUsageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return PeriodUsage{}, fmt.Errorf("JSON parse error (GetCurrentPeriodUsage): %w", err)
	}
	pu := wire.PlanUsage
	if pu == nil {
		pu = wire.PlanUsageAlt
	}
	if pu == nil {
		return PeriodUsage{}, fmt.Errorf("GetCurrentPeriodUsage: planUsage missing")
	}

	included := numInt(pu.IncludedSpend)
	bonus := numInt(pu.BonusSpend)
	totalSpend := numInt(firstNumber(pu.TotalSpend, pu.Spend))
	if totalSpend == 0 && (included > 0 || bonus > 0) {
		totalSpend = included + bonus
	}
	if included == 0 && totalSpend > 0 && bonus == 0 {
		// Older payloads sometimes expose totalSpend only.
		included = totalSpend
	}

	out := PeriodUsage{
		TotalPercent:       clampPercent(numFloat(firstNumber(pu.TotalPercentUsed, pu.TotalPercent))),
		AutoPercent:        clampPercent(numFloat(firstNumber(pu.AutoPercentUsed, pu.AutoPercent))),
		APIPercent:         clampPercent(numFloat(firstNumber(pu.APIPercentUsed, pu.APIPercent))),
		SpendCents:         totalSpend,
		IncludedSpendCents: included,
		BonusSpendCents:    bonus,
		LimitCents:         numInt(pu.Limit),
		RemainingCents:     numInt(pu.Remaining),
		OnDemand:           parseOnDemand(firstSpendLimit(wire.SpendLimitUsage, wire.SpendLimitAlt)),
	}
	if t, ok := parseTime(wire.BillingCycleStart); ok {
		out.CycleStart = &t
	}
	if t, ok := parseTime(wire.BillingCycleEnd); ok {
		out.CycleEnd = &t
	}
	return out, nil
}

func firstSpendLimit(a, b *spendLimitWire) *spendLimitWire {
	if a != nil {
		return a
	}
	return b
}

// parseOnDemand reads spendLimitUsage. The limit is pooled when limitType is
// team (or pooledLimit > 0 and the type is not "user"), otherwise individual.
// Returns nil when there is neither spend nor limit.
func parseOnDemand(slu *spendLimitWire) *OnDemandUsage {
	if slu == nil {
		return nil
	}
	limitType := strings.ToLower(strings.TrimSpace(slu.LimitType))
	pooledLimit := numInt(slu.PooledLimit)
	indivLimit := numInt(slu.IndividualLimit)

	usePooled := limitType == "team" || (limitType != "user" && pooledLimit > 0)

	var limit, remaining, usedFallback int64
	if usePooled {
		limit = pooledLimit
		remaining = numInt(slu.PooledRemaining)
		usedFallback = numInt(slu.PooledUsed)
		if limitType == "" {
			limitType = "team"
		}
	} else {
		limit = indivLimit
		remaining = numInt(slu.IndividualRemaining)
		usedFallback = numInt(slu.IndividualUsed)
		if limitType == "" && indivLimit > 0 {
			limitType = "user"
		}
	}

	spend := numInt(slu.TotalSpend)
	if !hasNumber(slu.TotalSpend) {
		spend = usedFallback
	}

	if spend == 0 && limit == 0 {
		return nil
	}

	out := &OnDemandUsage{
		SpendCents:     spend,
		LimitCents:     limit,
		RemainingCents: remaining,
		LimitType:      limitType,
	}
	if limit > 0 {
		out.Percent = clampPercent(float64(spend) * 100 / float64(limit))
	}
	return out
}

func hasNumber(n json.Number) bool {
	return strings.TrimSpace(n.String()) != ""
}

type planInfoParsed struct {
	PlanName            string
	IncludedAmountCents int64
	Price               string
}

type planInfoWire struct {
	PlanInfo *struct {
		PlanName            string      `json:"planName"`
		IncludedAmountCents json.Number `json:"includedAmountCents"`
		Price               string      `json:"price"`
	} `json:"planInfo"`
}

func parsePlanInfo(raw []byte) planInfoParsed {
	var wire planInfoWire
	if err := json.Unmarshal(raw, &wire); err != nil || wire.PlanInfo == nil {
		return planInfoParsed{}
	}
	return planInfoParsed{
		PlanName:            strings.TrimSpace(wire.PlanInfo.PlanName),
		IncludedAmountCents: numInt(wire.PlanInfo.IncludedAmountCents),
		Price:               strings.TrimSpace(wire.PlanInfo.Price),
	}
}

type grokUsageWire struct {
	UsagePercent            *float64 `json:"usagePercent"`
	HasNonZeroIncludedLimit *bool    `json:"hasNonZeroIncludedLimit"`
	IncludedLimitZero       *bool    `json:"includedLimitZero"`
	UsesPooledEnterprise    *bool    `json:"usesPooledEnterpriseAllowance"`
	NextResetTimestampUTC   flexTime `json:"nextResetTimestampUtc"`
	NextResetTimestampUTC2  flexTime `json:"nextResetTimestampUTC"`
}

func parseGrokUsage(raw []byte) (*GrokUsage, error) {
	var wire grokUsageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("JSON parse error (GetSandUsageStatus): %w", err)
	}

	hasLimit := true
	if wire.HasNonZeroIncludedLimit != nil {
		hasLimit = *wire.HasNonZeroIncludedLimit
	}
	if wire.IncludedLimitZero != nil && *wire.IncludedLimitZero {
		hasLimit = false
	}
	if wire.UsesPooledEnterprise != nil && *wire.UsesPooledEnterprise {
		hasLimit = false
	}
	if !hasLimit || wire.UsagePercent == nil {
		return nil, nil
	}

	g := &GrokUsage{UsagePercent: clampPercent(*wire.UsagePercent)}
	if t, ok := wire.NextResetTimestampUTC.Time(); ok {
		g.ResetsAt = &t
	} else if t, ok := wire.NextResetTimestampUTC2.Time(); ok {
		g.ResetsAt = &t
	}
	return g, nil
}

// flexTime accepts RFC3339, epoch seconds or epoch milliseconds (number or
// string).
type flexTime struct {
	t  time.Time
	ok bool
}

func (f *flexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if t, ok := parseTime(s); ok {
			f.t, f.ok = t, true
			return nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			f.t, f.ok = epochToTime(n), true
		}
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return nil
	}
	i, err := n.Int64()
	if err != nil {
		return nil
	}
	f.t, f.ok = epochToTime(i), true
	return nil
}

func (f flexTime) Time() (time.Time, bool) { return f.t, f.ok }

func epochToTime(n int64) time.Time {
	if n > 1_000_000_000_000 {
		return time.UnixMilli(n).UTC()
	}
	return time.Unix(n, 0).UTC()
}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func firstNumber(vals ...json.Number) json.Number {
	for _, v := range vals {
		if strings.TrimSpace(v.String()) != "" {
			return v
		}
	}
	return ""
}

func numFloat(n json.Number) float64 {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

func numInt(n json.Number) int64 {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
