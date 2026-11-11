package devin

import (
	"context"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/budget"
	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

// ProviderName is the bounded provider label for Devin.
const ProviderName = "devin"

// Provider collects one Devin subscription (one token).
type Provider struct {
	client       *Client
	subscription string
	now          func() time.Time
}

// NewProvider creates a Devin provider. subscription is a bounded label,
// normally "default".
func NewProvider(client *Client, subscription string) *Provider {
	return &Provider{client: client, subscription: subscription, now: time.Now}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return ProviderName }

// Subscriptions implements provider.Provider.
func (p *Provider) Subscriptions() []string { return []string{p.subscription} }

// Collect implements provider.Provider.
func (p *Provider) Collect(ctx context.Context) []provider.Result {
	start := time.Now()
	st, err := p.client.FetchStatus(ctx)
	duration := time.Since(start)
	res := provider.Result{Subscription: p.subscription, Duration: duration}
	if err != nil {
		res.Err = err
		return []provider.Result{res}
	}

	res.Data = snapshot.Subscription{
		Provider: ProviderName,
		Name:     p.subscription,
		Quotas:   BuildQuotas(st, p.now()),
		ACU:      BuildACU(st),
	}
	// A missing seat quota degrades the subscription but must not hide ACU
	// data already fetched from the REST API. Keep the real diagnostic for the
	// collector to sanitize and log; it never becomes a metric label.
	if st.QuotaError != "" {
		res.Partial = true
		res.Diagnostic = "quota: " + st.QuotaError
	}
	return []provider.Result{res}
}

// BuildQuotas maps Devin daily/weekly percentages onto the quota contract.
// Daily is a 24-hour period ending at its reset; weekly is seven days.
func BuildQuotas(st Status, now time.Time) []snapshot.Quota {
	if st.Quota == nil {
		return nil
	}
	var quotas []snapshot.Quota
	if st.Quota.DailyUsedPercent != nil {
		quotas = append(quotas, newQuota(snapshot.WindowDaily, *st.Quota.DailyUsedPercent, st.Quota.DailyResetsAt, budget.KindDaily, now))
	}
	if st.Quota.WeeklyUsedPercent != nil {
		quotas = append(quotas, newQuota(snapshot.WindowWeekly, *st.Quota.WeeklyUsedPercent, st.Quota.WeeklyResetsAt, budget.KindWeekly, now))
	}
	return quotas
}

func newQuota(window string, percent float64, reset *time.Time, kind string, now time.Time) snapshot.Quota {
	percent = clampPercent(percent)
	pct := int(percent + 0.5)
	// Devin seat quotas keep the legacy absolute thresholds. Pace metrics add
	// context but never change the meaning of their traffic light.
	level := budget.AbsoluteLevel(pct).Severity()
	q := snapshot.Quota{
		Window:    window,
		Quota:     snapshot.QuotaTotal,
		UsedRatio: percent / 100,
		Severity:  &level,
	}
	if reset != nil {
		q.ResetAt = *reset
	}
	b := budget.ComputeBudget(budget.Window{
		Kind:     kind,
		Percent:  pct,
		ResetsAt: q.ResetAt,
	}, now)
	if b.Valid {
		elapsed := float64(b.ElapsedPct) / 100
		dry := b.DryDays
		q.ElapsedRatio = &elapsed
		q.DryDays = &dry
	}
	return q
}

// BuildACU exposes total and per-product consumption. Per-product values are
// only published when the API returned at least one day; with no daily
// breakdown, only the known total is emitted.
func BuildACU(st Status) *snapshot.ACU {
	out := &snapshot.ACU{Consumed: st.ACUConsumed}
	if st.CycleStart != nil {
		out.CycleStart = *st.CycleStart
	}
	if st.CycleEnd != nil {
		out.CycleEnd = *st.CycleEnd
	}
	if len(st.Days) > 0 {
		products := map[string]float64{
			snapshot.ACUDevin:    0,
			snapshot.ACUCascade:  0,
			snapshot.ACUTerminal: 0,
		}
		for _, d := range st.Days {
			products[snapshot.ACUDevin] += d.ACUsByProduct.Devin
			products[snapshot.ACUCascade] += d.ACUsByProduct.Cascade
			products[snapshot.ACUTerminal] += d.ACUsByProduct.Terminal
		}
		out.ByProduct = products
	}
	return out
}
