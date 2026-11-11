// Package devin reads Devin ACU consumption and daily/weekly seat quotas.
package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Devin APIs — two hosts:
//
//	REST https://api.devin.ai
//	  GET /v3/self                                  -> identity (+ optional org_id)
//	  GET /v3/organizations/{org}/consumption/daily -> ACU consumption for the cycle
//
//	Connect-RPC https://server.codeium.com
//	  POST …/SeatManagementService/GetUserStatus    -> daily/weekly quotas (%)
//
// Three production lessons this client absorbs:
//
//  1. org_id is nullable and optional for both PatUserSelf and ServiceUserSelf:
//     a perfectly valid PAT may carry no organisation. No public endpoint lists
//     a PAT's organisations, so DEVIN_ORG_ID is the fallback.
//  2. Without time_after/time_before the endpoint returns the server's default
//     window, which need not match the billing cycle. The request is bounded to
//     the current cycle (see CycleBounds).
//  3. Daily/weekly percentages are only in GetUserStatus, not the REST API.
//     The same DEVIN_API_KEY (cog_) works as the Connect-RPC apiKey metadata.
//
// The absolute ACU plan limit is not exposed by the public REST API, so the
// exporter never publishes one.

const apiBaseURL = "https://api.devin.ai"

const (
	selfPath        = "/v3/self"
	consumptionPath = "/v3/organizations/%s/consumption/daily"
)

// orgPathPrefix is the only place where a 403/404 accuses DEVIN_ORG_ID rather
// than DEVIN_API_KEY.
const orgPathPrefix = "/v3/organizations/"

// defaultResetDay is the day of month when the individual Pro plan budget
// resets (observed in the UI; not exposed by the REST API).
const defaultResetDay = 5

// cycleBoundaryHourUTC is the fixed day boundary used by Devin billing:
// midnight PST = 08:00:00 UTC. PST, never PDT.
const cycleBoundaryHourUTC = 8

// SelfResponse is the response of GET /v3/self. Every field is optional.
type SelfResponse struct {
	PrincipalType string `json:"principal_type"`
	UserID        string `json:"user_id"`
	UserName      string `json:"user_name"`
	OrgID         string `json:"org_id"`
	APIKeyID      string `json:"api_key_id"`
	APIKeyName    string `json:"api_key_name"`
}

// ConsumptionResponse is the response of
// GET /v3/organizations/{org}/consumption/daily.
type ConsumptionResponse struct {
	TotalACUs         float64             `json:"total_acus"`
	ConsumptionByDate []ConsumptionByDate `json:"consumption_by_date"`
}

// ConsumptionByDate is one day of consumption.
type ConsumptionByDate struct {
	Date          int64         `json:"date"`
	ACUs          float64       `json:"acus"`
	ACUsByProduct ACUsByProduct `json:"acus_by_product"`
}

// ACUsByProduct details consumption per Devin product.
type ACUsByProduct struct {
	Devin    float64  `json:"devin"`
	Cascade  float64  `json:"cascade"`
	Terminal float64  `json:"terminal"`
	Review   *float64 `json:"review"`
}

// Status holds Devin ACU data plus optional daily/weekly seat quotas.
type Status struct {
	// ACUConsumed is total ACU consumed during the queried billing cycle.
	ACUConsumed float64    `json:"acuConsumed"`
	Days        []DayUsage `json:"days,omitempty"`
	DayCount    int        `json:"dayCount"`

	// OrgID is the organisation queried (DEVIN_ORG_ID or /v3/self).
	OrgID string `json:"orgId,omitempty"`

	// CycleStart/CycleEnd are pointers so an error status omits them rather
	// than publishing a zero timestamp.
	CycleStart *time.Time `json:"cycleStart,omitempty"`
	CycleEnd   *time.Time `json:"cycleEnd,omitempty"`

	// Quota is the daily/weekly percentages from GetUserStatus. Omitted when
	// the seat endpoint is unavailable, while ACU data is still returned.
	Quota *Quota `json:"quota,omitempty"`

	// QuotaError is a soft diagnostic when GetUserStatus fails.
	QuotaError string `json:"quotaError,omitempty"`

	FetchedAt time.Time `json:"fetchedAt"`
	Error     string    `json:"error,omitempty"`
}

// DayUsage is one day of ACU consumption.
type DayUsage struct {
	Date          time.Time     `json:"date"`
	ACUs          float64       `json:"acus"`
	ACUsByProduct ACUsByProduct `json:"acusByProduct"`
}

// Config is what the Devin client needs.
type Config struct {
	Token    string
	OrgID    string
	ResetDay int
	Timeout  time.Duration
	BaseURL  string
	SeatURL  string
}

// ConfigFromEnv reads Devin configuration. No variable is required: without
// DEVIN_API_KEY the provider is absent.
func ConfigFromEnv(timeout time.Duration) Config {
	return Config{
		Token:    TokenFromEnv(),
		OrgID:    strings.TrimSpace(os.Getenv("DEVIN_ORG_ID")),
		ResetDay: ResetDayFromEnv(),
		Timeout:  timeout,
		SeatURL:  strings.TrimSpace(os.Getenv("DEVIN_API_SERVER")),
	}
}

// Client calls the Devin REST API and the Windsurf seat endpoint.
type Client struct {
	http     *http.Client
	baseURL  string
	seatURL  string
	token    string
	orgID    string
	resetDay int
	now      func() time.Time
}

// NewClient creates a Devin client from its configuration.
func NewClient(cfg Config) *Client {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = apiBaseURL
	}
	seatURL := cfg.SeatURL
	if seatURL == "" {
		seatURL = defaultSeatBaseURL
	}
	return &Client{
		http:    &http.Client{Timeout: cfg.Timeout},
		baseURL: baseURL,
		seatURL: seatURL,
		// Defense in depth: a secret manager -> Kubernetes secret often carries
		// a trailing newline, and "Bearer cog_xxx\n" yields a misleading 401.
		token:    strings.TrimSpace(cfg.Token),
		orgID:    strings.TrimSpace(cfg.OrgID),
		resetDay: resetDayOrDefault(cfg.ResetDay),
		now:      time.Now,
	}
}

// FetchStatus fetches ACU consumption and, as a soft dependency, quotas.
func (c *Client) FetchStatus(ctx context.Context) (Status, error) {
	self, err := c.getJSON(ctx, selfPath)
	if err != nil {
		return Status{}, err
	}
	var sr SelfResponse
	if err := json.Unmarshal(self, &sr); err != nil {
		return Status{}, fmt.Errorf("JSON parse error (/v3/self): %w", err)
	}

	orgID, err := c.resolveOrg(sr)
	if err != nil {
		return Status{}, err
	}

	now := c.now()

	// Quotas are fetched first because planStart/planEnd are the real cycle
	// bounds. A GetUserStatus failure must not hide the ACU data: REST and seat
	// endpoints are distinct hosts.
	start, end := CycleBounds(now, c.resetDay) // local fallback
	var quota *Quota
	var quotaErr error
	if q, qerr := c.fetchQuota(ctx); qerr != nil {
		quotaErr = qerr
	} else {
		quota = &q
		if s, e, ok := planCycle(&q, now); ok {
			start, end = s, e
		}
	}
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}

	body, err := c.getJSON(ctx, consumptionURL(orgID, start, cappedEnd(end, now)))
	if err != nil {
		return Status{}, err
	}
	st, err := parseResponse(body, orgID, start, end)
	if err != nil {
		return Status{}, err
	}

	st.Quota = quota
	if quota == nil {
		if quotaErr != nil {
			st.QuotaError = quotaErr.Error()
		} else {
			st.QuotaError = "GetUserStatus unavailable"
		}
	}
	return st, nil
}

// planCycle returns planStart/planEnd when the cycle actually contains now.
func planCycle(q *Quota, now time.Time) (start, end time.Time, ok bool) {
	if q == nil || q.PlanStart == nil || q.PlanEnd == nil {
		return time.Time{}, time.Time{}, false
	}
	start, end = *q.PlanStart, *q.PlanEnd
	if !start.Before(end) || now.Before(start) || !now.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// resolveOrg picks the organisation: configuration first, then /v3/self.
func (c *Client) resolveOrg(sr SelfResponse) (string, error) {
	if c.orgID != "" {
		return c.orgID, nil
	}
	if org := strings.TrimSpace(sr.OrgID); org != "" {
		return org, nil
	}
	principal := sr.PrincipalType
	if principal == "" {
		principal = "unknown"
	}
	return "", fmt.Errorf(
		"no Devin organisation resolvable: /v3/self (principal_type=%s) has no org_id — "+
			"set DEVIN_ORG_ID=org-... or use a service-user token attached to the organisation",
		principal)
}

// consumptionURL bounds the request to the billing cycle.
func consumptionURL(orgID string, start, end time.Time) string {
	q := url.Values{}
	q.Set("time_after", strconv.FormatInt(start.Unix(), 10))
	q.Set("time_before", strconv.FormatInt(end.Unix(), 10))
	return fmt.Sprintf(consumptionPath, url.PathEscape(orgID)) + "?" + q.Encode()
}

// cappedEnd caps the upper bound at now. The current cycle end is in the
// future, and the endpoint documents a 422 for malformed bounds; capping
// removes the risk of a future bound failing the whole section.
func cappedEnd(end, now time.Time) time.Time {
	if now.Before(end) {
		return now
	}
	return end
}

// orgFromPath extracts the organisation from /v3/organizations/{org}/… .
func orgFromPath(path string) string {
	p, _, _ := strings.Cut(path, "?")
	rest, ok := strings.CutPrefix(p, orgPathPrefix)
	if !ok {
		return ""
	}
	org, _, _ := strings.Cut(rest, "/")
	if unescaped, err := url.PathUnescape(org); err == nil {
		return unescaped
	}
	return org
}

func (c *Client) getJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ai-usage-exporter/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		return body, nil

	case code == http.StatusForbidden || code == http.StatusNotFound:
		if org := orgFromPath(path); org != "" {
			return nil, c.orgInaccessibleErr(org, code)
		}
		if code == http.StatusNotFound {
			return nil, fmt.Errorf("devin HTTP %d: %s", code, truncate(string(body), 200))
		}
		return nil, c.tokenRefuseErr(code, path)

	case code == http.StatusUnauthorized:
		return nil, c.tokenRefuseErr(code, path)

	default:
		return nil, fmt.Errorf("devin HTTP %d: %s", code, truncate(string(body), 200))
	}
}

// orgInaccessibleErr distinguishes the two possible origins of the org: an
// explicit DEVIN_ORG_ID, or the org_id declared by /v3/self.
func (c *Client) orgInaccessibleErr(org string, code int) error {
	if c.orgID != "" {
		return fmt.Errorf(
			"devin organisation %s inaccessible (HTTP %d): it does not exist or the token cannot access it — check DEVIN_ORG_ID",
			org, code)
	}
	return fmt.Errorf(
		"devin organisation %s inaccessible (HTTP %d): this org came from /v3/self and DEVIN_ORG_ID is not set — "+
			"the token cannot access the organisation it declares, or it no longer exists",
		org, code)
}

// tokenRefuseErr diagnoses a rejected key without leaking it.
func (c *Client) tokenRefuseErr(code int, path string) error {
	return fmt.Errorf(
		"devin token rejected (HTTP %d) on %s: key missing, expired or out of scope — check DEVIN_API_KEY",
		code, path)
}

// parseResponse converts consumption/daily into Status.
func parseResponse(body []byte, orgID string, cycleStart, cycleEnd time.Time) (Status, error) {
	var resp ConsumptionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Status{}, fmt.Errorf("JSON parse error: %w", err)
	}

	s := Status{
		ACUConsumed: resp.TotalACUs,
		OrgID:       orgID,
		CycleStart:  &cycleStart,
		CycleEnd:    &cycleEnd,
		FetchedAt:   time.Now(),
	}

	for _, d := range resp.ConsumptionByDate {
		s.Days = append(s.Days, DayUsage{
			Date:          time.Unix(d.Date, 0),
			ACUs:          d.ACUs,
			ACUsByProduct: d.ACUsByProduct,
		})
	}
	s.DayCount = len(s.Days)
	return s, nil
}

// CycleBounds returns the billing cycle containing now: start is the latest
// resetDay at 08:00 UTC not after now, end is the next occurrence.
func CycleBounds(now time.Time, resetDay int) (start, end time.Time) {
	day := resetDayOrDefault(resetDay)
	year, month, _ := now.UTC().Date()

	start = time.Date(year, month, day, cycleBoundaryHourUTC, 0, 0, 0, time.UTC)
	if start.After(now) {
		start = time.Date(year, month-1, day, cycleBoundaryHourUTC, 0, 0, 0, time.UTC)
	}
	end = time.Date(start.Year(), start.Month()+1, day, cycleBoundaryHourUTC, 0, 0, 0, time.UTC)
	return start, end
}

// TokenFromEnv reads DEVIN_API_KEY (optional). Trimmed because a Kubernetes
// secret often carries a trailing newline.
func TokenFromEnv() string {
	return strings.TrimSpace(os.Getenv("DEVIN_API_KEY"))
}

// ResetDayFromEnv reads DEVIN_RESET_DAY. Out-of-range values fall back to 5.
func ResetDayFromEnv() int {
	return ResetDayFromValue(os.Getenv("DEVIN_RESET_DAY"))
}

// ResetDayFromValue parses DEVIN_RESET_DAY. Invalid or out-of-range values
// fall back to the known default 5.
func ResetDayFromValue(raw string) int {
	if v := strings.TrimSpace(raw); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return resetDayOrDefault(n)
		}
	}
	return defaultResetDay
}

// resetDayOrDefault only accepts [1,28]: beyond that, not every month has the
// day and the cycle would skip. Out-of-range falls back to the known default.
func resetDayOrDefault(day int) int {
	if day < 1 || day > 28 {
		return defaultResetDay
	}
	return day
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
