// Package snapshot defines the immutable data model produced by the
// collection loop and consumed by the Prometheus collector.
//
// Providers build plain values here; nothing in this package talks to an API,
// logs, or exposes a secret. The model is intentionally narrow so that the
// metric contract stays bounded: every label value has a known set.
package snapshot

import "time"

// Window values used by the quota metrics.
const (
	WindowMonthly   = "monthly"
	WindowWeekly    = "weekly"
	WindowRolling5h = "rolling_5h"
	WindowDaily     = "daily"
)

// Quota kinds used by the quota metrics. A provider only emits the kinds it
// actually exposes; labels are never derived from free-form API strings.
const (
	QuotaTotal    = "total"
	QuotaIncluded = "included"
	QuotaAPI      = "api"
	QuotaOnDemand = "on_demand"
	QuotaGrok     = "grok"
)

// Spend buckets used by the spend metrics.
const (
	SpendIncluded = "included"
	SpendOnDemand = "on_demand"
)

// ACU product labels used by Devin.
const (
	ACUAll      = "all"
	ACUDevin    = "devin"
	ACUCascade  = "cascade"
	ACUTerminal = "terminal"
)

// Snapshot is one complete, immutable view of every configured subscription.
// It is replaced atomically by the collector; readers never mutate it.
type Snapshot struct {
	// CollectedAt is when this snapshot was assembled.
	CollectedAt time.Time
	// Subscriptions are sorted by provider then subscription name.
	Subscriptions []Subscription
}

// Subscription is one billable account of one provider.
type Subscription struct {
	Provider string // opencode, cursor, devin
	Name     string // bounded label derived from configuration

	Quotas         []Quota
	MonthlyCostUSD *float64
	Spend          []Spend
	SpendLimits    []SpendLimit
	ACU            *ACU

	// Collection describes the health of the last collection attempt. Data
	// above is the last valid data, even when Success is false.
	Collection Collection
}

// Quota is one quota window/kind pair.
type Quota struct {
	Window    string // monthly, weekly, rolling_5h, daily
	Quota     string // total, included, api, on_demand, grok
	UsedRatio float64
	ResetAt   time.Time
	// ElapsedRatio, DryDays and Severity are nil when they cannot be derived
	// (notably the rolling 5h window, which has no period start).
	ElapsedRatio *float64
	DryDays      *float64
	Severity     *int // 0 green, 1 amber, 2 red
}

// Spend is a known amount in USD for a bounded bucket.
type Spend struct {
	Bucket string
	USD    float64
}

// SpendLimit is a known spend ceiling in USD for a bounded bucket.
type SpendLimit struct {
	Bucket string
	USD    float64
}

// ACU is Devin's ACU consumption. Limit is nil unless the API truly exposes
// an absolute plan limit (it generally does not).
type ACU struct {
	Consumed   float64
	ByProduct  map[string]float64
	Limit      *float64
	CycleStart time.Time
	CycleEnd   time.Time
}

// Collection is the outcome of the latest attempt for a subscription.
type Collection struct {
	Success bool
	// Partial marks a successful main collection with a degraded secondary
	// source (for example Grok or Devin seat quotas), so dashboards can show
	// it without hiding primary data.
	Partial     bool
	LastSuccess time.Time // unchanged when an attempt fails
	Duration    time.Duration
	Error       string // sanitized; never exposed as a label
}
