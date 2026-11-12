package cursor

import (
	"context"
	"strings"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/budget"
	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

// ProviderName is the bounded provider label for Cursor.
const ProviderName = "cursor"

// Provider collects one Cursor subscription (one credential pair).
type Provider struct {
	client       *Client
	subscription string
	now          func() time.Time
}

// NewProvider creates a Cursor provider for one credential. subscription is a
// bounded label derived from the credential's variable suffix, for example
// "b_personal" or "r_personal".
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

	data := snapshot.Subscription{
		Provider: ProviderName,
		Name:     p.subscription,
		Quotas:   BuildQuotas(st, p.now()),
	}
	addMoney(&data, st.Period)
	if st.PlanPrice != "" {
		if cost, ok := ParseMonthlyPriceUSD(st.PlanPrice); ok {
			data.MonthlyCostUSD = &cost
		}
	}
	res.Data = data
	var diagnostics []string
	if st.PlanError != "" {
		diagnostics = append(diagnostics, "plan: "+st.PlanError)
	}
	if st.GrokError != "" {
		diagnostics = append(diagnostics, "grok: "+st.GrokError)
	}
	res.Partial = len(diagnostics) > 0
	res.Diagnostic = strings.Join(diagnostics, "; ")
	return []provider.Result{res}
}

// BuildQuotas maps Cursor percentages onto the bounded quota contract.
//
// Cursor's "Total" is the overall monthly plan usage. "Auto" is the included
// plan allowance breakdown and is exposed as quota="included"; "API" is
// quota="api". On-demand usage is exposed as quota="on_demand" only when a
// real limit makes the ratio meaningful. Grok Bot is weekly/quota="grok".
func BuildQuotas(st Status, now time.Time) []snapshot.Quota {
	if st.Period == nil {
		return nil
	}
	p := st.Period
	var reset time.Time
	if p.CycleEnd != nil {
		reset = *p.CycleEnd
	}

	quotas := []snapshot.Quota{
		newMonthlyQuota(snapshot.QuotaTotal, p.TotalPercent, reset, now),
		newMonthlyQuota(snapshot.QuotaIncluded, p.AutoPercent, reset, now),
		newMonthlyQuota(snapshot.QuotaAPI, p.APIPercent, reset, now),
	}
	if od := p.OnDemand; od != nil && od.LimitCents > 0 {
		quotas = append(quotas, newMonthlyQuota(snapshot.QuotaOnDemand, od.Percent, reset, now))
	}
	if st.Grok != nil {
		var grokReset time.Time
		if st.Grok.ResetsAt != nil {
			grokReset = *st.Grok.ResetsAt
		}
		quotas = append(quotas, newQuota(snapshot.WindowWeekly, snapshot.QuotaGrok, st.Grok.UsagePercent, grokReset, budget.KindWeekly, now))
	}
	return quotas
}

func newMonthlyQuota(quota string, percent float64, reset time.Time, now time.Time) snapshot.Quota {
	return newQuota(snapshot.WindowMonthly, quota, percent, reset, budget.KindMonthly, now)
}

func newQuota(window, quota string, percent float64, reset time.Time, kind string, now time.Time) snapshot.Quota {
	percent = clampPercent(percent)
	pct := int(percent + 0.5)
	// Cursor bars historically use absolute consumption thresholds, including
	// when the provider omits a reset. Pace metrics are additional context and
	// must not replace that established severity.
	level := budget.AbsoluteLevel(pct).Severity()
	q := snapshot.Quota{
		Window:    window,
		Quota:     quota,
		UsedRatio: percent / 100,
		ResetAt:   reset,
		Severity:  &level,
	}
	b := budget.ComputeBudget(budget.Window{
		Kind:     kind,
		Percent:  pct,
		ResetsAt: reset,
	}, now)
	if b.Valid {
		elapsed := float64(b.ElapsedPct) / 100
		dry := b.DryDays
		q.ElapsedRatio = &elapsed
		q.DryDays = &dry
	}
	return q
}

func addMoney(data *snapshot.Subscription, p *PeriodUsage) {
	if p == nil {
		return
	}

	includedCents := p.IncludedSpendCents
	if includedCents == 0 {
		includedCents = p.SpendCents
	}
	if includedCents > 0 || p.LimitCents > 0 {
		data.Spend = append(data.Spend, snapshot.Spend{
			Bucket: snapshot.SpendIncluded,
			USD:    float64(includedCents) / 100,
		})
		if p.LimitCents > 0 {
			data.SpendLimits = append(data.SpendLimits, snapshot.SpendLimit{
				Bucket: snapshot.SpendIncluded,
				USD:    float64(p.LimitCents) / 100,
			})
		}
	}

	if od := p.OnDemand; od != nil {
		data.Spend = append(data.Spend, snapshot.Spend{
			Bucket: snapshot.SpendOnDemand,
			USD:    float64(od.SpendCents) / 100,
		})
		if od.LimitCents > 0 {
			data.SpendLimits = append(data.SpendLimits, snapshot.SpendLimit{
				Bucket: snapshot.SpendOnDemand,
				USD:    float64(od.LimitCents) / 100,
			})
		}
	}
}
