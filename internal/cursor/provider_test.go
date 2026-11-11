package cursor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

func snapshotSubscription() snapshot.Subscription {
	return snapshot.Subscription{Provider: "cursor", Name: "default"}
}

func testTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBuildQuotasMapping(t *testing.T) {
	cycleEnd := testTime("2026-10-01T00:00:00Z")
	grokReset := testTime("2026-09-19T00:00:00Z")
	st := Status{
		Period: &PeriodUsage{
			TotalPercent: 42.5,
			AutoPercent:  30,
			APIPercent:   12.5,
			CycleEnd:     &cycleEnd,
			OnDemand:     &OnDemandUsage{SpendCents: 1500, LimitCents: 5000, Percent: 30},
		},
		Grok: &GrokUsage{UsagePercent: 18.5, ResetsAt: &grokReset},
	}
	now := testTime("2026-09-15T00:00:00Z")
	quotas := BuildQuotas(st, now)
	if len(quotas) != 5 {
		t.Fatalf("quotas = %+v", quotas)
	}

	want := map[string]struct {
		window string
		ratio  float64
	}{
		"total":     {"monthly", 0.425},
		"included":  {"monthly", 0.30},
		"api":       {"monthly", 0.125},
		"on_demand": {"monthly", 0.30},
		"grok":      {"weekly", 0.185},
	}
	for _, q := range quotas {
		w, ok := want[q.Quota]
		if !ok {
			t.Errorf("unexpected quota %+v", q)
			continue
		}
		if q.Window != w.window {
			t.Errorf("%s window = %q, want %q", q.Quota, q.Window, w.window)
		}
		if q.UsedRatio != w.ratio {
			t.Errorf("%s ratio = %v, want %v", q.Quota, q.UsedRatio, w.ratio)
		}
		if q.ResetAt.IsZero() {
			t.Errorf("%s reset is zero", q.Quota)
		}
		if q.Severity == nil {
			t.Errorf("%s severity missing", q.Quota)
		}
	}
}

func TestBuildQuotasSeverityUsesAbsoluteLevel(t *testing.T) {
	reset := testTime("2026-10-01T00:00:00Z")
	now := testTime("2026-09-03T00:00:00Z")
	q := newMonthlyQuota(snapshot.QuotaTotal, 60, reset, now)
	if q.DryDays == nil || *q.DryDays <= 3 {
		t.Fatalf("test setup should produce a pace-driven red budget: %+v", q)
	}
	if q.Severity == nil || *q.Severity != 0 {
		t.Errorf("severity = %v, want absolute green despite pace", q.Severity)
	}

	withoutReset := newMonthlyQuota(snapshot.QuotaTotal, 95, time.Time{}, now)
	if withoutReset.Severity == nil || *withoutReset.Severity != 2 {
		t.Errorf("severity without reset = %v, want absolute red", withoutReset.Severity)
	}
}

func TestBuildQuotasOnDemandRequiresKnownLimit(t *testing.T) {
	st := Status{Period: &PeriodUsage{
		TotalPercent: 10,
		OnDemand:     &OnDemandUsage{SpendCents: 250, LimitCents: 0},
	}}
	quotas := BuildQuotas(st, time.Now())
	if len(quotas) != 3 {
		t.Fatalf("quotas = %+v, want total/included/api only", quotas)
	}
	for _, q := range quotas {
		if q.Quota == "on_demand" {
			t.Errorf("on_demand quota emitted without a limit: %+v", q)
		}
	}
}

func TestBuildQuotasNilPeriod(t *testing.T) {
	if quotas := BuildQuotas(Status{}, time.Now()); quotas != nil {
		t.Errorf("quotas = %+v, want nil", quotas)
	}
}

func TestAddMoney(t *testing.T) {
	data := snapshotSubscription()
	addMoney(&data, &PeriodUsage{
		IncludedSpendCents: 4250,
		LimitCents:         10000,
		OnDemand:           &OnDemandUsage{SpendCents: 1500, LimitCents: 5000},
	})

	if len(data.Spend) != 2 {
		t.Fatalf("spend = %+v", data.Spend)
	}
	if data.Spend[0].Bucket != "included" || data.Spend[0].USD != 42.5 {
		t.Errorf("included spend = %+v", data.Spend[0])
	}
	if data.Spend[1].Bucket != "on_demand" || data.Spend[1].USD != 15 {
		t.Errorf("on-demand spend = %+v", data.Spend[1])
	}
	if len(data.SpendLimits) != 2 || data.SpendLimits[0].USD != 100 || data.SpendLimits[1].USD != 50 {
		t.Errorf("limits = %+v", data.SpendLimits)
	}
}

func TestAddMoneySkipsUnknownLimits(t *testing.T) {
	data := snapshotSubscription()
	addMoney(&data, &PeriodUsage{
		IncludedSpendCents: 100,
		OnDemand:           &OnDemandUsage{SpendCents: 250},
	})
	if len(data.Spend) != 2 {
		t.Errorf("spend = %+v", data.Spend)
	}
	if len(data.SpendLimits) != 0 {
		t.Errorf("limits = %+v, want none", data.SpendLimits)
	}
}

func TestProviderCollectAndMonthlyCost(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	p := NewProvider(s.clientWithAPIKey(t, "crsr_test"), "default")

	results := p.Collect(context.Background())
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	res := results[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Subscription != "default" || res.Data.Provider != "cursor" {
		t.Errorf("labels = %+v", res)
	}
	if res.Partial {
		t.Error("Partial = true, want false")
	}
	if res.Data.MonthlyCostUSD == nil || *res.Data.MonthlyCostUSD != 20 {
		t.Errorf("MonthlyCostUSD = %v, want 20", res.Data.MonthlyCostUSD)
	}
	if len(res.Data.Quotas) != 5 {
		t.Errorf("quotas = %+v", res.Data.Quotas)
	}
}

func TestProviderCollectPartialOnGrokFailure(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.sandStatus = 500
	s.sandWebStatus = 500
	p := NewProvider(s.clientWithAPIKey(t, "crsr_test"), "default")

	res := p.Collect(context.Background())[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.Partial {
		t.Error("Partial = false, want true when Grok is unavailable")
	}
	if !strings.Contains(res.Diagnostic, "grok:") {
		t.Errorf("Diagnostic = %q, want a Grok diagnostic", res.Diagnostic)
	}
	if len(res.Data.Quotas) != 4 {
		t.Errorf("primary quotas = %+v, want total/included/api/on_demand", res.Data.Quotas)
	}
}

func TestProviderUnknownPriceIsNotInvented(t *testing.T) {
	s := newCursorServer(t)
	s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
	s.planBody = `{"planInfo":{"planName":"Pro","price":"$240/year"}}`
	p := NewProvider(s.clientWithAPIKey(t, "crsr_test"), "default")

	res := p.Collect(context.Background())[0]
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Data.MonthlyCostUSD != nil {
		t.Errorf("MonthlyCostUSD = %v, want nil for an annual price", *res.Data.MonthlyCostUSD)
	}
}
