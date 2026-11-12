package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Daily/weekly quotas are not exposed by the public REST API. They live in the
// Connect-RPC SeatManagementService/GetUserStatus on server.codeium.com — the
// same endpoint as the Devin CLI and OpenUsage.
//
//	POST /exa.seat_management_pb.SeatManagementService/GetUserStatus
//	body: {"metadata":{"apiKey":"<DEVIN_API_KEY>","ideName":"devin",...}}
//	-> planStatus.dailyQuotaRemainingPercent, dailyQuotaResetAtUnix,
//	   weeklyQuotaResetAtUnix (weeklyQuotaRemainingPercent sometimes absent
//	   on Pro), planInfo.planName, planStart/planEnd.
//
// Percentages are stored as "used" (100 - remaining): 0 = nothing consumed,
// 100 = ceiling. Protobuf JSON omits zero-valued fields, so a fully consumed
// window arrives with its reset timestamp but without the percentage: a
// present reset with no percentage means 100% used, not "unknown".

const (
	defaultSeatBaseURL = "https://server.codeium.com"
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
	seatCompatVersion  = "1.108.2"
)

// Quota holds daily/weekly percentages from GetUserStatus.
type Quota struct {
	PlanName string `json:"planName,omitempty"`

	// DailyUsedPercent is nil when the plan hides the daily quota
	// (hideDailyQuota) or the field is absent.
	DailyUsedPercent *float64   `json:"dailyUsedPercent,omitempty"`
	DailyResetsAt    *time.Time `json:"dailyResetsAt,omitempty"`

	// WeeklyUsedPercent is nil when the API omits it (observed on Pro). No
	// value is invented from the daily quota.
	WeeklyUsedPercent *float64   `json:"weeklyUsedPercent,omitempty"`
	WeeklyResetsAt    *time.Time `json:"weeklyResetsAt,omitempty"`

	// PlanStart/PlanEnd are the real billing cycle bounds.
	PlanStart *time.Time `json:"planStart,omitempty"`
	PlanEnd   *time.Time `json:"planEnd,omitempty"`

	// OnDemandBalanceUSD is the remaining pay-as-you-go balance, converted
	// from overageBalanceMicros. Nil when absent.
	OnDemandBalanceUSD *float64 `json:"onDemandBalanceUSD,omitempty"`

	// BillingStrategy is for example BILLING_STRATEGY_QUOTA (Pro: quotas + $,
	// so consumption/daily REST is empty).
	BillingStrategy string `json:"billingStrategy,omitempty"`
}

type seatUserStatusResponse struct {
	UserStatus struct {
		PlanStatus seatPlanStatus `json:"planStatus"`
	} `json:"userStatus"`
}

type seatPlanStatus struct {
	PlanInfo                    seatPlanInfo `json:"planInfo"`
	PlanStart                   string       `json:"planStart"`
	PlanEnd                     string       `json:"planEnd"`
	DailyQuotaRemainingPercent  *float64     `json:"dailyQuotaRemainingPercent"`
	WeeklyQuotaRemainingPercent *float64     `json:"weeklyQuotaRemainingPercent"`
	DailyQuotaResetAtUnix       json.Number  `json:"dailyQuotaResetAtUnix"`
	WeeklyQuotaResetAtUnix      json.Number  `json:"weeklyQuotaResetAtUnix"`
	OverageBalanceMicros        json.Number  `json:"overageBalanceMicros"`
}

type seatPlanInfo struct {
	PlanName        string `json:"planName"`
	HideDailyQuota  bool   `json:"hideDailyQuota"`
	BillingStrategy string `json:"billingStrategy"`
}

type seatRequestBody struct {
	Metadata seatMetadata `json:"metadata"`
}

type seatMetadata struct {
	APIKey           string `json:"apiKey"`
	IDEName          string `json:"ideName"`
	IDEVersion       string `json:"ideVersion"`
	ExtensionName    string `json:"extensionName"`
	ExtensionVersion string `json:"extensionVersion"`
	Locale           string `json:"locale"`
}

// fetchQuota queries GetUserStatus. Failures are soft in FetchStatus.
func (c *Client) fetchQuota(ctx context.Context) (Quota, error) {
	body, err := json.Marshal(seatRequestBody{Metadata: seatMetadata{
		APIKey:           c.token,
		IDEName:          "devin",
		IDEVersion:       seatCompatVersion,
		ExtensionName:    "devin",
		ExtensionVersion: seatCompatVersion,
		Locale:           "en",
	}})
	if err != nil {
		return Quota{}, err
	}

	url := strings.TrimRight(c.seatURL, "/") + seatUserStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Quota{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ai-usage-exporter/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return Quota{}, fmt.Errorf("network error (GetUserStatus): %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Quota{}, fmt.Errorf("read error (GetUserStatus): %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Quota{}, fmt.Errorf("GetUserStatus rejected (HTTP %d): check DEVIN_API_KEY", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return Quota{}, fmt.Errorf("GetUserStatus HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	return parseQuota(raw)
}

func parseQuota(raw []byte) (Quota, error) {
	var resp seatUserStatusResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Quota{}, fmt.Errorf("JSON parse error (GetUserStatus): %w", err)
	}
	ps := resp.UserStatus.PlanStatus
	if ps.PlanInfo.PlanName == "" && ps.DailyQuotaRemainingPercent == nil && ps.WeeklyQuotaRemainingPercent == nil {
		return Quota{}, fmt.Errorf("GetUserStatus: empty planStatus")
	}

	q := Quota{
		PlanName:        strings.TrimSpace(ps.PlanInfo.PlanName),
		BillingStrategy: strings.TrimSpace(ps.PlanInfo.BillingStrategy),
	}
	if t, ok := parseRFC3339(ps.PlanStart); ok {
		q.PlanStart = &t
	}
	if t, ok := parseRFC3339(ps.PlanEnd); ok {
		q.PlanEnd = &t
	}
	if t, ok := unixNumber(ps.DailyQuotaResetAtUnix); ok {
		q.DailyResetsAt = &t
	}
	if t, ok := unixNumber(ps.WeeklyQuotaResetAtUnix); ok {
		q.WeeklyResetsAt = &t
	}

	if !ps.PlanInfo.HideDailyQuota {
		q.DailyUsedPercent = usedPercent(ps.DailyQuotaRemainingPercent, ps.DailyQuotaResetAtUnix)
	}
	q.WeeklyUsedPercent = usedPercent(ps.WeeklyQuotaRemainingPercent, ps.WeeklyQuotaResetAtUnix)

	if usd, ok := microsUSD(ps.OverageBalanceMicros); ok {
		q.OnDemandBalanceUSD = &usd
	}
	return q, nil
}

// usedPercent converts a remaining percentage into a consumed percentage.
// A nil remaining value with a valid reset means the window is exhausted:
// protobuf JSON omits zero. Without a reset there is no evidence of the
// window and no value is invented.
func usedPercent(remaining *float64, reset json.Number) *float64 {
	if remaining != nil {
		used := clampPercent(100 - *remaining)
		return &used
	}
	if _, ok := unixNumber(reset); ok {
		used := 100.0
		return &used
	}
	return nil
}

// microsUSD converts millionths of a dollar (JSON number or string) to USD.
func microsUSD(n json.Number) (float64, bool) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return 0, false
	}
	micros, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(micros) / 1_000_000, true
}

func parseRFC3339(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// unixNumber accepts a JSON number or string Unix timestamp.
func unixNumber(n json.Number) (time.Time, bool) {
	s := strings.TrimSpace(n.String())
	if s == "" {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
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
