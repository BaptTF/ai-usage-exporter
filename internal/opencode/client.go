// Package opencode reads the OpenCode Go usage API.
package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/budget"
)

const defaultAPIURL = "https://opencode.ai/zen/go/v1/usage"

// Window is a single quota window (rolling, weekly, monthly).
type Window = budget.Window

// Window kinds. Each kind has its own period-start rule, verified against real
// API responses:
//
//	rolling — genuinely rolling; with zero usage the API returns now+5h, so no
//	          period start can be derived.
//	weekly  — calendar-aligned: every key resets at the same Monday 00:00 UTC.
//	monthly — subscription anniversary: arbitrary day-of-month and time-of-day,
//	          different for every key.
const (
	KindRolling = budget.KindRolling
	KindWeekly  = budget.KindWeekly
	KindMonthly = budget.KindMonthly
)

// apiResponse matches the real API response format.
type apiResponse struct {
	Usage struct {
		Rolling *windowRaw `json:"rolling"`
		Weekly  *windowRaw `json:"weekly"`
		Monthly *windowRaw `json:"monthly"`
	} `json:"usage"`
}

type windowRaw struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// Client calls the OpenCode Go usage API.
type Client struct {
	http    *http.Client
	baseURL string
}

// NewClient creates a Client with the given timeout.
func NewClient(timeout time.Duration) *Client {
	return NewClientWithURL(timeout, defaultAPIURL)
}

// NewClientWithURL creates a Client pointing at a custom base URL (for tests
// and for a future self-hosted endpoint).
func NewClientWithURL(timeout time.Duration, baseURL string) *Client {
	return &Client{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// FetchUsage retrieves usage data for the given API key. The key is only sent
// in the Authorization header and is never included in returned errors.
func (c *Client) FetchUsage(ctx context.Context, apiKey string) ([]Window, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
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

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("OpenCode key rejected (HTTP %d): check OPENCODE_GO_API_KEY*", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("OpenCode rate-limited (HTTP 429)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenCode HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	return parseResponse(body)
}

func parseResponse(body []byte) ([]Window, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("JSON parse error: %w", err)
	}

	var windows []Window

	// Monthly first: it is the window that actually constrains the month.
	if w := resp.Usage.Monthly; w != nil {
		windows = append(windows, toWindow("Monthly", KindMonthly, w))
	}
	if w := resp.Usage.Weekly; w != nil {
		windows = append(windows, toWindow("Weekly", KindWeekly, w))
	}
	if w := resp.Usage.Rolling; w != nil {
		windows = append(windows, toWindow("Rolling 5h", KindRolling, w))
	}

	if len(windows) == 0 {
		return nil, fmt.Errorf("no usage window in OpenCode response")
	}

	return windows, nil
}

func toWindow(name, kind string, raw *windowRaw) Window {
	w := Window{
		Name:    name,
		Kind:    kind,
		Status:  raw.Status,
		Percent: raw.Percent,
	}
	if t, err := time.Parse(time.RFC3339Nano, raw.ResetsAt); err == nil {
		w.ResetsAt = t
	} else if t, err := time.Parse(time.RFC3339, raw.ResetsAt); err == nil {
		w.ResetsAt = t
	}
	return w
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
