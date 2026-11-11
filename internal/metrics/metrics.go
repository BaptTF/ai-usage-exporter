// Package metrics exposes the snapshot as Prometheus metrics.
//
// A full snapshot is rendered on every scrape, so series that disappear from
// the model disappear from the exposition too. Labels come only from bounded
// configuration or the fixed contract enums; error strings are never labels.
package metrics

import (
	"math"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

// Metric names documented in the README.
const (
	quotaUsedRatioName     = "ai_usage_quota_used_ratio"
	quotaResetName         = "ai_usage_quota_reset_timestamp_seconds"
	quotaElapsedRatioName  = "ai_usage_quota_elapsed_ratio"
	quotaDryDaysName       = "ai_usage_quota_dry_days"
	quotaSeverityName      = "ai_usage_quota_budget_severity"
	monthlyCostName        = "ai_usage_subscription_monthly_cost_usd"
	spendName              = "ai_usage_spend_usd"
	spendLimitName         = "ai_usage_spend_limit_usd"
	acuConsumedName        = "ai_usage_acu_consumed"
	acuLimitName           = "ai_usage_acu_limit"
	acuCycleStartName      = "ai_usage_acu_cycle_start_timestamp_seconds"
	acuCycleResetName      = "ai_usage_acu_cycle_reset_timestamp_seconds"
	collectionSuccessName  = "ai_usage_collection_success"
	collectionLastOkName   = "ai_usage_collection_last_success_timestamp_seconds"
	collectionDurationName = "ai_usage_collection_duration_seconds"
	collectionPartialName  = "ai_usage_collection_partial"
)

var (
	quotaLabels     = []string{"provider", "subscription", "window", "quota"}
	subLabels       = []string{"provider", "subscription"}
	spendLabels     = []string{"provider", "subscription", "bucket"}
	acuLabels       = []string{"provider", "subscription", "product"}
	allowedWindows  = map[string]bool{snapshot.WindowMonthly: true, snapshot.WindowWeekly: true, snapshot.WindowRolling5h: true, snapshot.WindowDaily: true}
	allowedQuotas   = map[string]bool{snapshot.QuotaTotal: true, snapshot.QuotaIncluded: true, snapshot.QuotaAPI: true, snapshot.QuotaOnDemand: true, snapshot.QuotaGrok: true}
	allowedSpends   = map[string]bool{snapshot.SpendIncluded: true, snapshot.SpendOnDemand: true}
	acuProductOrder = []string{snapshot.ACUAll, snapshot.ACUDevin, snapshot.ACUCascade, snapshot.ACUTerminal}
	allowedACU      = map[string]bool{snapshot.ACUAll: true, snapshot.ACUDevin: true, snapshot.ACUCascade: true, snapshot.ACUTerminal: true}
)

// Collector implements prometheus.Collector over a snapshot source.
type Collector struct {
	source func() *snapshot.Snapshot

	quotaUsed     *prometheus.Desc
	quotaReset    *prometheus.Desc
	quotaElapsed  *prometheus.Desc
	quotaDryDays  *prometheus.Desc
	quotaSeverity *prometheus.Desc
	monthlyCost   *prometheus.Desc
	spend         *prometheus.Desc
	spendLimit    *prometheus.Desc
	acuConsumed   *prometheus.Desc
	acuLimit      *prometheus.Desc
	acuCycleStart *prometheus.Desc
	acuCycleReset *prometheus.Desc
	collSuccess   *prometheus.Desc
	collLastOK    *prometheus.Desc
	collDuration  *prometheus.Desc
	collPartial   *prometheus.Desc
	descs         []*prometheus.Desc
}

// New creates a collector reading the latest snapshot from source.
func New(source func() *snapshot.Snapshot) *Collector {
	c := &Collector{source: source}
	c.quotaUsed = prometheus.NewDesc(quotaUsedRatioName,
		"Fraction of a quota window consumed, from 0 to 1.", quotaLabels, nil)
	c.quotaReset = prometheus.NewDesc(quotaResetName,
		"Unix timestamp at which the quota window resets.", quotaLabels, nil)
	c.quotaElapsed = prometheus.NewDesc(quotaElapsedRatioName,
		"Fraction of the quota period elapsed, from 0 to 1; absent when not derivable.", quotaLabels, nil)
	c.quotaDryDays = prometheus.NewDesc(quotaDryDaysName,
		"Days spent at the quota ceiling before the reset at the current average rate; absent when not derivable.", quotaLabels, nil)
	c.quotaSeverity = prometheus.NewDesc(quotaSeverityName,
		"Quota budget severity: 0 green, 1 amber, 2 red.", quotaLabels, nil)
	c.monthlyCost = prometheus.NewDesc(monthlyCostName,
		"Known monthly subscription cost in USD.", subLabels, nil)
	c.spend = prometheus.NewDesc(spendName,
		"Known spend in USD for a bounded bucket.", spendLabels, nil)
	c.spendLimit = prometheus.NewDesc(spendLimitName,
		"Known spend limit in USD for a bounded bucket.", spendLabels, nil)
	c.acuConsumed = prometheus.NewDesc(acuConsumedName,
		"Devin ACU consumed during the queried billing cycle.", acuLabels, nil)
	c.acuLimit = prometheus.NewDesc(acuLimitName,
		"Known absolute ACU plan limit; absent when the provider does not expose one.", subLabels, nil)
	c.acuCycleStart = prometheus.NewDesc(acuCycleStartName,
		"Unix timestamp at which the queried Devin ACU billing cycle started.", subLabels, nil)
	c.acuCycleReset = prometheus.NewDesc(acuCycleResetName,
		"Unix timestamp at which the queried Devin ACU billing cycle resets.", subLabels, nil)
	c.collSuccess = prometheus.NewDesc(collectionSuccessName,
		"Whether the last collection attempt succeeded (1) or used stale data (0).", subLabels, nil)
	c.collLastOK = prometheus.NewDesc(collectionLastOkName,
		"Unix timestamp of the last successful collection for this subscription.", subLabels, nil)
	c.collDuration = prometheus.NewDesc(collectionDurationName,
		"Duration of the last collection attempt in seconds.", subLabels, nil)
	c.collPartial = prometheus.NewDesc(collectionPartialName,
		"Whether the last successful collection had a degraded secondary source (1) or not (0).", subLabels, nil)

	c.descs = []*prometheus.Desc{
		c.quotaUsed, c.quotaReset, c.quotaElapsed, c.quotaDryDays, c.quotaSeverity,
		c.monthlyCost, c.spend, c.spendLimit, c.acuConsumed, c.acuLimit,
		c.acuCycleStart, c.acuCycleReset, c.collSuccess, c.collLastOK, c.collDuration, c.collPartial,
	}
	return c
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range c.descs {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if c.source == nil {
		return
	}
	snap := c.source()
	if snap == nil {
		return
	}
	seen := make(map[string]struct{})
	for _, sub := range snap.Subscriptions {
		c.collectSubscription(ch, sub, seen)
	}
}

func (c *Collector) collectSubscription(ch chan<- prometheus.Metric, sub snapshot.Subscription, seen map[string]struct{}) {
	provider, name := sub.Provider, sub.Name
	labels := []string{provider, name}

	success := 0.0
	if sub.Collection.Success {
		success = 1
	}
	c.emit(ch, collectionSuccessName, c.collSuccess, success, seen, labels...)

	partial := 0.0
	if sub.Collection.Success && sub.Collection.Partial {
		partial = 1
	}
	c.emit(ch, collectionPartialName, c.collPartial, partial, seen, labels...)

	if !sub.Collection.LastSuccess.IsZero() {
		if ts := sub.Collection.LastSuccess.Unix(); ts > 0 {
			c.emit(ch, collectionLastOkName, c.collLastOK, float64(ts), seen, labels...)
		}
	}

	duration := sub.Collection.Duration.Seconds()
	if duration < 0 {
		duration = 0
	}
	c.emit(ch, collectionDurationName, c.collDuration, duration, seen, labels...)

	if sub.MonthlyCostUSD != nil {
		c.emit(ch, monthlyCostName, c.monthlyCost, *sub.MonthlyCostUSD, seen, labels...)
	}

	for _, q := range sub.Quotas {
		if !allowedWindows[q.Window] || !allowedQuotas[q.Quota] {
			continue
		}
		qlabels := []string{provider, name, q.Window, q.Quota}
		c.emit(ch, quotaUsedRatioName, c.quotaUsed, clamp01(q.UsedRatio), seen, qlabels...)
		if !q.ResetAt.IsZero() {
			if ts := q.ResetAt.Unix(); ts > 0 {
				c.emit(ch, quotaResetName, c.quotaReset, float64(ts), seen, qlabels...)
			}
		}
		if q.ElapsedRatio != nil {
			c.emit(ch, quotaElapsedRatioName, c.quotaElapsed, clamp01(*q.ElapsedRatio), seen, qlabels...)
		}
		if q.DryDays != nil {
			dry := *q.DryDays
			if dry < 0 {
				dry = 0
			}
			c.emit(ch, quotaDryDaysName, c.quotaDryDays, dry, seen, qlabels...)
		}
		if q.Severity != nil {
			level := *q.Severity
			if level < 0 {
				level = 0
			}
			if level > 2 {
				level = 2
			}
			c.emit(ch, quotaSeverityName, c.quotaSeverity, float64(level), seen, qlabels...)
		}
	}

	for _, s := range sub.Spend {
		if !allowedSpends[s.Bucket] {
			continue
		}
		c.emit(ch, spendName, c.spend, s.USD, seen, provider, name, s.Bucket)
	}
	for _, s := range sub.SpendLimits {
		if !allowedSpends[s.Bucket] {
			continue
		}
		c.emit(ch, spendLimitName, c.spendLimit, s.USD, seen, provider, name, s.Bucket)
	}

	if sub.ACU != nil {
		c.emit(ch, acuConsumedName, c.acuConsumed, sub.ACU.Consumed, seen, provider, name, snapshot.ACUAll)
		for _, product := range acuProductOrder {
			if product == snapshot.ACUAll {
				continue
			}
			value, ok := sub.ACU.ByProduct[product]
			if !ok || !allowedACU[product] {
				continue
			}
			c.emit(ch, acuConsumedName, c.acuConsumed, value, seen, provider, name, product)
		}
		if sub.ACU.Limit != nil {
			c.emit(ch, acuLimitName, c.acuLimit, *sub.ACU.Limit, seen, provider, name)
		}
		if !sub.ACU.CycleStart.IsZero() {
			c.emit(ch, acuCycleStartName, c.acuCycleStart, float64(sub.ACU.CycleStart.Unix()), seen, provider, name)
		}
		if !sub.ACU.CycleEnd.IsZero() {
			c.emit(ch, acuCycleResetName, c.acuCycleReset, float64(sub.ACU.CycleEnd.Unix()), seen, provider, name)
		}
	}
}

// emit writes one gauge unless the value is non-finite or the same series was
// already emitted. Duplicate quota rows are dropped defensively so a gather
// never fails on duplicate labels.
func (c *Collector) emit(ch chan<- prometheus.Metric, metricName string, desc *prometheus.Desc, value float64, seen map[string]struct{}, labels ...string) {
	if !isFinite(value) || value < 0 {
		return
	}
	key := metricName
	for _, label := range labels {
		key += "\x00" + label
	}
	if _, ok := seen[key]; ok {
		return
	}
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	if err != nil {
		return
	}
	seen[key] = struct{}{}
	ch <- m
}

func clamp01(v float64) float64 {
	if !isFinite(v) {
		return math.NaN() // emit skips it; never publish NaN or invent 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
