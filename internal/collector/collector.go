// Package collector runs the polling loop and keeps the last valid snapshot.
//
// Providers are collected in parallel and independently. A failed attempt
// never erases the previous valid data for that subscription: only its
// collection health changes, and last_success stays at the last successful
// attempt. This is what makes /metrics scrape-safe during an API outage.
package collector

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

const maxErrorLength = 500

type subscriptionKey struct {
	provider string
	name     string
}

type state struct {
	data        snapshot.Subscription
	success     bool
	partial     bool
	lastSuccess time.Time
	duration    time.Duration
	err         string
}

type providerMeta struct {
	name     string
	subs     []string
	provider provider.Provider
}

// Collector owns the polling loop and the current snapshot.
type Collector struct {
	providers []providerMeta
	secrets   []string
	logger    *log.Logger
	now       func() time.Time

	mu     sync.Mutex
	order  []subscriptionKey
	states map[subscriptionKey]*state

	snap atomic.Pointer[snapshot.Snapshot]
}

// New validates the provider/subscription set and seeds collection metrics so
// /metrics is usable before the first poll completes. Duplicate subscription
// labels within one provider are rejected: they would produce duplicate
// Prometheus series.
func New(providers []provider.Provider, secrets []string, logger *log.Logger) (*Collector, error) {
	c := &Collector{
		secrets: append([]string(nil), secrets...),
		logger:  logger,
		now:     time.Now,
		states:  make(map[subscriptionKey]*state),
	}
	for _, p := range providers {
		name := strings.TrimSpace(p.Name())
		if name == "" {
			return nil, fmt.Errorf("provider with an empty name")
		}
		rawSubs := p.Subscriptions()
		subs := make([]string, 0, len(rawSubs))
		for _, sub := range rawSubs {
			sub = strings.TrimSpace(sub)
			if sub == "" {
				return nil, fmt.Errorf("provider %s has an empty subscription name", name)
			}
			k := subscriptionKey{provider: name, name: sub}
			if _, ok := c.states[k]; ok {
				return nil, fmt.Errorf("duplicate subscription %s/%s", name, sub)
			}
			c.states[k] = &state{
				data: snapshot.Subscription{Provider: name, Name: sub},
			}
			c.order = append(c.order, k)
			subs = append(subs, sub)
		}
		c.providers = append(c.providers, providerMeta{name: name, subs: subs, provider: p})
	}
	sort.Slice(c.order, func(i, j int) bool {
		if c.order[i].provider != c.order[j].provider {
			return c.order[i].provider < c.order[j].provider
		}
		return c.order[i].name < c.order[j].name
	})

	c.snap.Store(c.buildSnapshot(c.now()))
	return c, nil
}

// SetClock injects a clock for tests.
func (c *Collector) SetClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

// Snapshot returns the latest immutable snapshot. It is always safe to call,
// including from a concurrent scrape.
func (c *Collector) Snapshot() *snapshot.Snapshot {
	if s := c.snap.Load(); s != nil {
		return s
	}
	return &snapshot.Snapshot{}
}

// Run polls immediately, then at every interval until ctx is cancelled.
// Collection runs in the caller's goroutine so a slow provider never stacks
// overlapping polls, and cancellation always wins over a ready ticker: no new
// collection is started once ctx is done.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	if ctx.Err() != nil {
		return
	}
	c.CollectOnce(ctx)
	if ctx.Err() != nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// Both cases can be ready at once; check cancellation explicitly so a
		// shutdown never starts one more HTTP round.
		if ctx.Err() != nil {
			return
		}
		c.CollectOnce(ctx)
	}
}

type providerOutcome struct {
	name    string
	subs    []string
	results []provider.Result
	panic   bool
	dur     time.Duration
}

// CollectOnce fetches every provider in parallel and publishes a new snapshot.
// A cancelled context aborts before starting and discards in-flight results,
// preserving the previous snapshot instead of recording cancellation as an
// upstream failure.
func (c *Collector) CollectOnce(ctx context.Context) *snapshot.Snapshot {
	if ctx.Err() != nil {
		return c.Snapshot()
	}
	outcomes := make([]providerOutcome, len(c.providers))
	var wg sync.WaitGroup
	for i, p := range c.providers {
		wg.Add(1)
		go func(idx int, p providerMeta) {
			defer wg.Done()
			out := providerOutcome{name: p.name, subs: p.subs}
			start := time.Now()
			func() {
				defer func() {
					if recover() != nil {
						out.panic = true
					}
				}()
				out.results = p.provider.Collect(ctx)
			}()
			out.dur = time.Since(start)
			outcomes[idx] = out
		}(i, p)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return c.Snapshot()
	}

	collectedAt := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, out := range outcomes {
		if out.panic {
			for _, sub := range out.subs {
				c.fail(subscriptionKey{provider: out.name, name: sub}, out.dur, "provider panicked", false)
			}
			continue
		}
		seen := make(map[string]bool, len(out.results))
		for _, res := range out.results {
			k := subscriptionKey{provider: out.name, name: strings.TrimSpace(res.Subscription)}
			if _, ok := c.states[k]; !ok {
				c.logf("ignoring unexpected collection result provider=%s subscription=%s", out.name, k.name)
				continue
			}
			seen[k.name] = true
			if res.Err != nil {
				c.fail(k, res.Duration, res.Err.Error(), res.Partial)
				continue
			}
			c.succeed(k, res, collectedAt)
		}
		// A provider that silently forgets a configured subscription must not
		// leave a stale success=true forever.
		for _, sub := range out.subs {
			if !seen[sub] {
				c.fail(subscriptionKey{provider: out.name, name: sub}, out.dur, "provider returned no result", false)
			}
		}
	}

	snap := c.buildSnapshot(collectedAt)
	c.snap.Store(snap)
	return snap
}

func (c *Collector) fail(k subscriptionKey, duration time.Duration, message string, partial bool) {
	st := c.states[k]
	st.success = false
	st.partial = partial
	if duration < 0 {
		duration = 0
	}
	st.duration = duration
	st.err = sanitize(message, c.secrets)
	c.logf("collection failed provider=%s subscription=%s error=%s", k.provider, k.name, st.err)
}

func (c *Collector) succeed(k subscriptionKey, res provider.Result, at time.Time) {
	st := c.states[k]
	data := normalize(res.Data, k)
	st.data = data
	st.success = true
	st.partial = res.Partial
	st.lastSuccess = at
	if res.Duration < 0 {
		res.Duration = 0
	}
	st.duration = res.Duration
	st.err = ""
	if res.Partial {
		diagnostic := sanitize(res.Diagnostic, c.secrets)
		if diagnostic == "" {
			diagnostic = "secondary source degraded without a diagnostic"
		}
		c.logf("collection partial provider=%s subscription=%s (primary data preserved) diagnostic=%s",
			k.provider, k.name, diagnostic)
	}
}

// normalize copies the provider data, forces the configured labels, and sorts
// the slices so snapshots are deterministic. Copying keeps previous snapshots
// immutable if a provider reuses a slice.
func normalize(in snapshot.Subscription, k subscriptionKey) snapshot.Subscription {
	out := in
	out.Provider = k.provider
	out.Name = k.name
	out.Quotas = append([]snapshot.Quota(nil), in.Quotas...)
	out.Spend = append([]snapshot.Spend(nil), in.Spend...)
	out.SpendLimits = append([]snapshot.SpendLimit(nil), in.SpendLimits...)
	if in.MonthlyCostUSD != nil {
		v := *in.MonthlyCostUSD
		out.MonthlyCostUSD = &v
	}
	if in.ACU != nil {
		acu := *in.ACU
		if in.ACU.Limit != nil {
			v := *in.ACU.Limit
			acu.Limit = &v
		}
		if in.ACU.ByProduct != nil {
			acu.ByProduct = make(map[string]float64, len(in.ACU.ByProduct))
			for name, value := range in.ACU.ByProduct {
				acu.ByProduct[name] = value
			}
		}
		out.ACU = &acu
	}
	sort.Slice(out.Quotas, func(i, j int) bool {
		if out.Quotas[i].Window != out.Quotas[j].Window {
			return out.Quotas[i].Window < out.Quotas[j].Window
		}
		return out.Quotas[i].Quota < out.Quotas[j].Quota
	})
	sort.Slice(out.Spend, func(i, j int) bool { return out.Spend[i].Bucket < out.Spend[j].Bucket })
	sort.Slice(out.SpendLimits, func(i, j int) bool { return out.SpendLimits[i].Bucket < out.SpendLimits[j].Bucket })
	return out
}

// buildSnapshot must be called with c.mu held (or before the collector is
// shared, as in New).
func (c *Collector) buildSnapshot(at time.Time) *snapshot.Snapshot {
	s := &snapshot.Snapshot{CollectedAt: at, Subscriptions: make([]snapshot.Subscription, 0, len(c.order))}
	for _, k := range c.order {
		st := c.states[k]
		sub := st.data
		if sub.Provider == "" {
			sub.Provider = k.provider
		}
		if sub.Name == "" {
			sub.Name = k.name
		}
		sub.Collection = snapshot.Collection{
			Success:     st.success,
			Partial:     st.partial,
			LastSuccess: st.lastSuccess,
			Duration:    st.duration,
			Error:       st.err,
		}
		s.Subscriptions = append(s.Subscriptions, sub)
	}
	return s
}

func (c *Collector) logf(format string, args ...any) {
	if c.logger == nil {
		return
	}
	c.logger.Printf(format, args...)
}

var (
	bearerPattern     = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	jwtPattern        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)
	knownTokenPattern = regexp.MustCompile(`\b(?:crsr_|cog_|sk-)[A-Za-z0-9._-]{8,}`)
)

// sanitize redacts known secret values and generic credential shapes before an
// error is stored or logged. Errors are never used as metric labels.
func sanitize(message string, secrets []string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	message = bearerPattern.ReplaceAllString(message, "Bearer [redacted]")
	message = jwtPattern.ReplaceAllString(message, "[redacted]")
	message = knownTokenPattern.ReplaceAllString(message, "[redacted]")
	// Keep each collection failure on one log line.
	message = strings.ReplaceAll(message, "\r", " ")
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > maxErrorLength {
		message = message[:maxErrorLength] + "..."
	}
	return message
}
