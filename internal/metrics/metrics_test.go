package metrics

import (
	"bytes"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

func ptrFloat(v float64) *float64 { return &v }
func ptrInt(v int) *int           { return &v }

func familyText(t *testing.T, families []*dto.MetricFamily) string {
	t.Helper()
	var buf bytes.Buffer
	for _, mf := range families {
		if _, err := expfmt.MetricFamilyToText(&buf, mf); err != nil {
			t.Fatalf("encode %s: %v", mf.GetName(), err)
		}
	}
	return buf.String()
}

func keyFor(name string, m *dto.Metric) string {
	parts := make([]string, 0, len(m.Label))
	for _, lp := range m.Label {
		parts = append(parts, lp.GetName()+"="+lp.GetValue())
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func gatherValues(t *testing.T, source func() *snapshot.Snapshot) (map[string]float64, []*dto.MetricFamily) {
	t.Helper()
	reg := prometheus.NewRegistry()
	reg.MustRegister(New(source))
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]float64)
	for _, mf := range families {
		for _, m := range mf.Metric {
			out[keyFor(mf.GetName(), m)] = m.GetGauge().GetValue()
		}
	}
	return out, families
}

func sampleSnapshot() *snapshot.Snapshot {
	reset := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	lastSuccess := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	cycleStart := time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)
	cycleEnd := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	cost := 20.0
	return &snapshot.Snapshot{
		Subscriptions: []snapshot.Subscription{
			{
				Provider: "opencode",
				Name:     "main",
				Quotas: []snapshot.Quota{{
					Window: "monthly", Quota: "total", UsedRatio: 0.5, ResetAt: reset,
					ElapsedRatio: ptrFloat(0.4), DryDays: ptrFloat(2.5), Severity: ptrInt(1),
				}},
				Collection: snapshot.Collection{
					Success: true, Partial: true, LastSuccess: lastSuccess,
					Duration: 2 * time.Second,
				},
			},
			{
				Provider:       "cursor",
				Name:           "r_personal",
				MonthlyCostUSD: &cost,
				Spend: []snapshot.Spend{
					{Bucket: "included", USD: 42.5},
					{Bucket: "on_demand", USD: 15},
				},
				SpendLimits: []snapshot.SpendLimit{
					{Bucket: "included", USD: 100},
					{Bucket: "on_demand", USD: 50},
				},
				Collection: snapshot.Collection{Success: true, LastSuccess: lastSuccess, Duration: time.Second},
			},
			{
				Provider: "devin",
				Name:     "default",
				ACU: &snapshot.ACU{
					Consumed:   12.5,
					CycleStart: cycleStart,
					CycleEnd:   cycleEnd,
					ByProduct: map[string]float64{
						snapshot.ACUDevin:    9,
						snapshot.ACUCascade:  3,
						snapshot.ACUTerminal: 0.5,
					},
				},
				Collection: snapshot.Collection{Success: true, LastSuccess: lastSuccess, Duration: 3 * time.Second},
			},
		},
	}
}

func TestContractMetrics(t *testing.T) {
	values, families := gatherValues(t, sampleSnapshot)

	want := map[string]float64{
		`ai_usage_quota_used_ratio{provider=opencode,quota=total,subscription=main,window=monthly}`:              0.5,
		`ai_usage_quota_reset_timestamp_seconds{provider=opencode,quota=total,subscription=main,window=monthly}`: float64(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix()),
		`ai_usage_quota_elapsed_ratio{provider=opencode,quota=total,subscription=main,window=monthly}`:           0.4,
		`ai_usage_quota_dry_days{provider=opencode,quota=total,subscription=main,window=monthly}`:                2.5,
		`ai_usage_quota_budget_severity{provider=opencode,quota=total,subscription=main,window=monthly}`:         1,
		`ai_usage_collection_success{provider=opencode,subscription=main}`:                                       1,
		`ai_usage_collection_partial{provider=opencode,subscription=main}`:                                       1,
		`ai_usage_collection_duration_seconds{provider=opencode,subscription=main}`:                              2,
		`ai_usage_subscription_monthly_cost_usd{provider=cursor,subscription=r_personal}`:                        20,
		`ai_usage_spend_usd{bucket=included,provider=cursor,subscription=r_personal}`:                            42.5,
		`ai_usage_spend_usd{bucket=on_demand,provider=cursor,subscription=r_personal}`:                           15,
		`ai_usage_spend_limit_usd{bucket=included,provider=cursor,subscription=r_personal}`:                      100,
		`ai_usage_spend_limit_usd{bucket=on_demand,provider=cursor,subscription=r_personal}`:                     50,
		`ai_usage_acu_consumed{product=all,provider=devin,subscription=default}`:                                 12.5,
		`ai_usage_acu_consumed{product=devin,provider=devin,subscription=default}`:                               9,
		`ai_usage_acu_consumed{product=cascade,provider=devin,subscription=default}`:                             3,
		`ai_usage_acu_consumed{product=terminal,provider=devin,subscription=default}`:                            0.5,
		`ai_usage_acu_cycle_start_timestamp_seconds{provider=devin,subscription=default}`:                        float64(time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC).Unix()),
		`ai_usage_acu_cycle_reset_timestamp_seconds{provider=devin,subscription=default}`:                        float64(time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC).Unix()),
	}
	for key, wantValue := range want {
		got, ok := values[key]
		if !ok {
			t.Errorf("missing metric %s", key)
			continue
		}
		if got != wantValue {
			t.Errorf("%s = %v, want %v", key, got, wantValue)
		}
	}

	// Timestamp series exist and match the reset/last-success instants.
	if got := values[`ai_usage_collection_last_success_timestamp_seconds{provider=opencode,subscription=main}`]; got != float64(time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC).Unix()) {
		t.Errorf("last_success = %v", got)
	}

	// Every family must carry HELP and TYPE in the exposition.
	text := familyText(t, families)
	for _, family := range families {
		if !strings.Contains(text, "# HELP "+family.GetName()+" ") {
			t.Errorf("missing HELP for %s", family.GetName())
		}
		if !strings.Contains(text, "# TYPE "+family.GetName()+" gauge") {
			t.Errorf("missing TYPE for %s", family.GetName())
		}
	}
	if strings.Contains(text, "NaN") || strings.Contains(text, "Inf") {
		t.Errorf("exposition contains NaN/Inf:\n%s", text)
	}
}

func TestSeriesDisappearWhenRemoved(t *testing.T) {
	snap := sampleSnapshot()
	values, _ := gatherValues(t, func() *snapshot.Snapshot { return snap })
	if _, ok := values[`ai_usage_quota_used_ratio{provider=opencode,quota=total,subscription=main,window=monthly}`]; !ok {
		t.Fatal("initial quota series missing")
	}

	snap = &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider: "opencode", Name: "main",
		Collection: snapshot.Collection{Success: true},
	}}}
	values, _ = gatherValues(t, func() *snapshot.Snapshot { return snap })
	if _, ok := values[`ai_usage_quota_used_ratio{provider=opencode,quota=total,subscription=main,window=monthly}`]; ok {
		t.Error("stale quota series still present")
	}
	if _, ok := values[`ai_usage_quota_reset_timestamp_seconds{provider=opencode,quota=total,subscription=main,window=monthly}`]; ok {
		t.Error("stale reset series still present")
	}
}

func TestNonFiniteAndUnboundedValuesAreSkipped(t *testing.T) {
	nanCost := math.NaN()
	snap := &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider:       "fake",
		Name:           "x",
		MonthlyCostUSD: &nanCost,
		Quotas: []snapshot.Quota{
			{Window: "monthly", Quota: "total", UsedRatio: math.NaN(), ElapsedRatio: ptrFloat(math.Inf(1)), DryDays: ptrFloat(math.NaN()), Severity: ptrInt(7)},
			{Window: "bogus", Quota: "total", UsedRatio: 0.5},
			{Window: "monthly", Quota: "bogus", UsedRatio: 0.5},
		},
		Spend:       []snapshot.Spend{{Bucket: "bogus", USD: 1}, {Bucket: "included", USD: -1}},
		SpendLimits: []snapshot.SpendLimit{{Bucket: "bogus", USD: 1}},
		ACU: &snapshot.ACU{Consumed: math.Inf(1), ByProduct: map[string]float64{
			"review":          99,
			snapshot.ACUDevin: math.NaN(),
		}},
		Collection: snapshot.Collection{Success: true},
	}}}
	values, families := gatherValues(t, func() *snapshot.Snapshot { return snap })
	text := familyText(t, families)
	if strings.Contains(text, "NaN") || strings.Contains(text, "Inf") {
		t.Errorf("exposition contains NaN/Inf:\n%s", text)
	}
	for key := range values {
		if strings.Contains(key, "bogus") || strings.Contains(key, "review") {
			t.Errorf("unbounded label leaked: %s", key)
		}
	}
	// Severity is bounded even if a provider sends out-of-range input.
	if got := values[`ai_usage_quota_budget_severity{provider=fake,quota=total,subscription=x,window=monthly}`]; got != 2 {
		t.Errorf("severity = %v, want clamped 2", got)
	}
	// The NaN used ratio must not appear.
	if _, ok := values[`ai_usage_quota_used_ratio{provider=fake,quota=total,subscription=x,window=monthly}`]; ok {
		t.Error("NaN used ratio was published")
	}
	if _, ok := values[`ai_usage_subscription_monthly_cost_usd{provider=fake,subscription=x}`]; ok {
		t.Error("NaN cost was published")
	}
}

func TestDuplicateQuotaRowsAreDeduplicated(t *testing.T) {
	sev := 0
	snap := &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider: "p", Name: "s",
		Quotas: []snapshot.Quota{
			{Window: "monthly", Quota: "total", UsedRatio: 0.1, Severity: &sev},
			{Window: "monthly", Quota: "total", UsedRatio: 0.9, Severity: &sev},
		},
		Collection: snapshot.Collection{Success: true},
	}}}
	values, err := func() (map[string]float64, error) {
		reg := prometheus.NewRegistry()
		reg.MustRegister(New(func() *snapshot.Snapshot { return snap }))
		families, err := reg.Gather()
		out := map[string]float64{}
		for _, mf := range families {
			for _, m := range mf.Metric {
				out[keyFor(mf.GetName(), m)] = m.GetGauge().GetValue()
			}
		}
		return out, err
	}()
	if err != nil {
		t.Fatalf("gather with duplicate rows: %v", err)
	}
	if got := values[`ai_usage_quota_used_ratio{provider=p,quota=total,subscription=s,window=monthly}`]; got != 0.1 {
		t.Errorf("first duplicate wins = %v, want 0.1", got)
	}
}

func TestPartialIsZeroWhenCollectionFailed(t *testing.T) {
	snap := &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider: "p", Name: "s",
		Collection: snapshot.Collection{Success: false, Partial: true, Error: "secret-value"},
	}}}
	values, families := gatherValues(t, func() *snapshot.Snapshot { return snap })
	if got := values[`ai_usage_collection_partial{provider=p,subscription=s}`]; got != 0 {
		t.Errorf("partial = %v, want 0 on hard failure", got)
	}
	if got := values[`ai_usage_collection_success{provider=p,subscription=s}`]; got != 0 {
		t.Errorf("success = %v, want 0", got)
	}
	if _, ok := values[`ai_usage_collection_last_success_timestamp_seconds{provider=p,subscription=s}`]; ok {
		t.Error("last_success emitted for a never-successful subscription")
	}
	if strings.Contains(familyText(t, families), "secret-value") {
		t.Error("collection error leaked into metrics")
	}
}

func TestConcurrentGathersAndSnapshotSwap(t *testing.T) {
	var current atomic.Pointer[snapshot.Snapshot]
	current.Store(sampleSnapshot())
	reg := prometheus.NewRegistry()
	reg.MustRegister(New(func() *snapshot.Snapshot { return current.Load() }))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := reg.Gather(); err != nil {
					t.Errorf("gather: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			current.Store(&snapshot.Snapshot{})
		} else {
			current.Store(sampleSnapshot())
		}
	}
	wg.Wait()
}

func TestOptionalDerivedSeries(t *testing.T) {
	snap := &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider: "opencode", Name: "main",
		Quotas: []snapshot.Quota{{
			Window: "rolling_5h", Quota: "total", UsedRatio: 0.91, Severity: ptrInt(2),
		}},
		Collection: snapshot.Collection{Success: true},
	}}}
	values, _ := gatherValues(t, func() *snapshot.Snapshot { return snap })
	if _, ok := values[`ai_usage_quota_elapsed_ratio{provider=opencode,quota=total,subscription=main,window=rolling_5h}`]; ok {
		t.Error("elapsed ratio present for rolling window")
	}
	if _, ok := values[`ai_usage_quota_dry_days{provider=opencode,quota=total,subscription=main,window=rolling_5h}`]; ok {
		t.Error("dry days present for rolling window")
	}
	if got := values[`ai_usage_quota_budget_severity{provider=opencode,quota=total,subscription=main,window=rolling_5h}`]; got != 2 {
		t.Errorf("rolling severity = %v", got)
	}
}
