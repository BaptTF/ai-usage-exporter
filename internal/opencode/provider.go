package opencode

import (
	"context"
	"sync"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/budget"
	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

// ProviderName is the bounded provider label for OpenCode.
const ProviderName = "opencode"

// Provider collects every configured OpenCode subscription in parallel.
type Provider struct {
	client *Client
	keys   []KeyConfig
	now    func() time.Time
}

// NewProvider creates an OpenCode provider. Keys may be empty when another
// provider is configured; the exporter refuses a fully empty configuration at
// startup, not here.
func NewProvider(client *Client, keys []KeyConfig) *Provider {
	return &Provider{client: client, keys: keys, now: time.Now}
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return ProviderName }

// Subscriptions implements provider.Provider.
func (p *Provider) Subscriptions() []string {
	out := make([]string, 0, len(p.keys))
	for _, k := range p.keys {
		out = append(out, k.Label)
	}
	return out
}

// Collect implements provider.Provider. Each key is fetched independently so
// one bad key cannot suppress the others.
func (p *Provider) Collect(ctx context.Context) []provider.Result {
	results := make([]provider.Result, len(p.keys))
	var wg sync.WaitGroup
	for i, kc := range p.keys {
		wg.Add(1)
		go func(idx int, kc KeyConfig) {
			defer wg.Done()
			start := time.Now()
			windows, err := p.client.FetchUsage(ctx, kc.Key)
			res := provider.Result{
				Subscription: kc.Label,
				Duration:     time.Since(start),
			}
			if err != nil {
				res.Err = err
			} else {
				res.Data = snapshot.Subscription{
					Provider: ProviderName,
					Name:     kc.Label,
					Quotas:   BuildQuotas(windows, p.now()),
				}
			}
			results[idx] = res
		}(i, kc)
	}
	wg.Wait()
	return results
}

// BuildQuotas converts API windows into the bounded metric model. Monthly and
// weekly windows use the pace budget; rolling 5h is graded on raw consumption
// only and has no elapsed/dry-day value.
func BuildQuotas(windows []Window, now time.Time) []snapshot.Quota {
	quotas := make([]snapshot.Quota, 0, len(windows))
	for _, w := range windows {
		pct := w.Percent
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		q := snapshot.Quota{
			Window:    metricWindow(w.Kind),
			Quota:     snapshot.QuotaTotal,
			UsedRatio: float64(pct) / 100,
		}
		if !w.ResetsAt.IsZero() {
			q.ResetAt = w.ResetsAt
		}

		if w.Kind == KindRolling {
			level := budget.AbsoluteLevel(pct).Severity()
			q.Severity = &level
		} else {
			b := budget.ComputeBudget(Window{
				Name:     w.Name,
				Kind:     w.Kind,
				Percent:  pct,
				ResetsAt: w.ResetsAt,
			}, now)
			if b.Valid {
				elapsed := float64(b.ElapsedPct) / 100
				dry := b.DryDays
				level := b.Level.Severity()
				q.ElapsedRatio = &elapsed
				q.DryDays = &dry
				q.Severity = &level
			} else {
				// Legacy fallback: without a usable reset there is no pace, so
				// grade the window on raw consumption only. elapsed/dry stay
				// absent rather than being invented.
				level := budget.AbsoluteLevel(pct).Severity()
				q.Severity = &level
			}
		}
		quotas = append(quotas, q)
	}
	return quotas
}

func metricWindow(kind string) string {
	switch kind {
	case KindMonthly:
		return snapshot.WindowMonthly
	case KindWeekly:
		return snapshot.WindowWeekly
	case KindRolling:
		return snapshot.WindowRolling5h
	default:
		return kind
	}
}
