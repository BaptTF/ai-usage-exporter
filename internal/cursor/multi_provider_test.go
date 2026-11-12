package cursor

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/BaptTF/ai-usage-exporter/internal/collector"
	"github.com/BaptTF/ai-usage-exporter/internal/metrics"
	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

func gatherCursorMetrics(t *testing.T, source func() *snapshot.Snapshot) (map[string]float64, string) {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.New(source))
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	values := make(map[string]float64)
	var text strings.Builder
	for _, family := range families {
		if _, err := expfmt.MetricFamilyToText(&text, family); err != nil {
			t.Fatalf("encode %s: %v", family.GetName(), err)
		}
		for _, metric := range family.Metric {
			labels := make([]string, 0, len(metric.Label))
			for _, pair := range metric.Label {
				labels = append(labels, pair.GetName()+"="+pair.GetValue())
			}
			sort.Strings(labels)
			key := family.GetName()
			if len(labels) > 0 {
				key += "{" + strings.Join(labels, ",") + "}"
			}
			values[key] = metric.GetGauge().GetValue()
		}
	}
	return values, text.String()
}

func TestMultipleCursorCredentialsCollectIndependently(t *testing.T) {
	type credential struct {
		label string
		key   string
		price string
	}
	credentials := []credential{
		{label: "b_personal", key: "crsr_b_personal_secret", price: "20"},
		{label: "b_student", key: "crsr_b_student_secret", price: "19"},
		{label: "r_personal", key: "crsr_r_personal_secret", price: "10"},
	}

	var arrived atomic.Int32
	var sequential atomic.Bool
	allArrived := make(chan struct{})
	periodHook := func() {
		if arrived.Add(1) == int32(len(credentials)) {
			close(allArrived)
		}
		select {
		case <-allArrived:
		case <-time.After(2 * time.Second):
			// A sequential collector reaches this branch for each provider.
			sequential.Store(true)
		}
	}

	servers := make(map[string]*cursorServer, len(credentials))
	providers := make([]provider.Provider, 0, len(credentials))
	secrets := make([]string, 0, len(credentials))
	for _, cred := range credentials {
		s := newCursorServer(t)
		s.exchangeBody = `{"accessToken":"` + accessJWT(t) + `"}`
		s.planBody = fmt.Sprintf(
			`{"planInfo":{"planName":"Pro","includedAmountCents":10000,"price":"$%s/mo"}}`,
			cred.price)
		s.periodHook = periodHook
		servers[cred.label] = s
		providers = append(providers, NewProvider(s.clientWithAPIKey(t, cred.key), cred.label))
		secrets = append(secrets, cred.key)
	}

	var logBuf bytes.Buffer
	coll, err := collector.New(providers, secrets, log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}

	first := coll.CollectOnce(context.Background())
	if sequential.Load() {
		t.Error("Cursor credentials were collected sequentially")
	}
	if len(first.Subscriptions) != len(credentials) {
		t.Fatalf("subscriptions = %+v", first.Subscriptions)
	}

	wantLabels := map[string]bool{
		"b_personal": true,
		"b_student":  true,
		"r_personal": true,
	}
	wantPrices := map[string]float64{
		"b_personal": 20,
		"b_student":  19,
		"r_personal": 10,
	}
	firstSuccess := make(map[string]time.Time, len(credentials))
	for _, sub := range first.Subscriptions {
		if sub.Provider != "cursor" {
			t.Errorf("provider = %q, want cursor", sub.Provider)
		}
		if sub.Name == "default" {
			t.Error("default Cursor subscription emitted")
		}
		if !wantLabels[sub.Name] {
			t.Errorf("unexpected subscription %q", sub.Name)
			continue
		}
		if !sub.Collection.Success {
			t.Errorf("%s did not succeed: %+v", sub.Name, sub.Collection)
		}
		firstSuccess[sub.Name] = sub.Collection.LastSuccess
		if sub.MonthlyCostUSD == nil || *sub.MonthlyCostUSD != wantPrices[sub.Name] {
			t.Errorf("%s monthly cost = %v, want %v", sub.Name, sub.MonthlyCostUSD, wantPrices[sub.Name])
		}
		if len(sub.Quotas) != 5 {
			t.Errorf("%s quotas = %+v, want 5", sub.Name, sub.Quotas)
		}
		if len(sub.Spend) != 2 || len(sub.SpendLimits) != 2 {
			t.Errorf("%s spend = %+v limits = %+v", sub.Name, sub.Spend, sub.SpendLimits)
		}
	}

	values, text := gatherCursorMetrics(t, coll.Snapshot)
	for label := range wantLabels {
		for _, metric := range []string{
			"ai_usage_collection_success",
			"ai_usage_collection_partial",
			"ai_usage_collection_duration_seconds",
			"ai_usage_collection_last_success_timestamp_seconds",
		} {
			key := metric + "{provider=cursor,subscription=" + label + "}"
			if _, ok := values[key]; !ok {
				t.Errorf("missing metric %s", key)
			}
		}
		if got := values["ai_usage_collection_success{provider=cursor,subscription="+label+"}"]; got != 1 {
			t.Errorf("%s collection_success = %v, want 1", label, got)
		}
		if got := values["ai_usage_collection_partial{provider=cursor,subscription="+label+"}"]; got != 0 {
			t.Errorf("%s collection_partial = %v, want 0", label, got)
		}
		if got := values["ai_usage_subscription_monthly_cost_usd{provider=cursor,subscription="+label+"}"]; got != wantPrices[label] {
			t.Errorf("%s monthly cost metric = %v, want %v", label, got, wantPrices[label])
		}
		for _, bucket := range []string{"included", "on_demand"} {
			key := "ai_usage_spend_usd{bucket=" + bucket + ",provider=cursor,subscription=" + label + "}"
			if _, ok := values[key]; !ok {
				t.Errorf("missing metric %s", key)
			}
			key = "ai_usage_spend_limit_usd{bucket=" + bucket + ",provider=cursor,subscription=" + label + "}"
			if _, ok := values[key]; !ok {
				t.Errorf("missing metric %s", key)
			}
		}
		for _, quota := range []struct{ name, window string }{
			{"total", "monthly"},
			{"included", "monthly"},
			{"api", "monthly"},
			{"on_demand", "monthly"},
			{"grok", "weekly"},
		} {
			for _, metric := range []string{
				"ai_usage_quota_used_ratio",
				"ai_usage_quota_reset_timestamp_seconds",
				"ai_usage_quota_budget_severity",
			} {
				key := metric + "{provider=cursor,quota=" + quota.name +
					",subscription=" + label + ",window=" + quota.window + "}"
				if _, ok := values[key]; !ok {
					t.Errorf("missing metric %s", key)
				}
			}
		}
	}
	for key := range values {
		if strings.Contains(key, "subscription=default") {
			t.Errorf("default Cursor subscription leaked into metrics: %s", key)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("metrics leak a Cursor credential: %s", text)
		}
	}

	// Fail exactly one credential. The other two must stay fresh, and the
	// failed one must keep its last valid data with success=0.
	failed := servers["b_student"]
	failed.periodStatus = http.StatusInternalServerError
	failed.periodBody = "boom crsr_b_student_secret"

	second := coll.CollectOnce(context.Background())
	if len(second.Subscriptions) != len(credentials) {
		t.Fatalf("subscriptions = %+v", second.Subscriptions)
	}
	for _, sub := range second.Subscriptions {
		switch sub.Name {
		case "b_student":
			if sub.Collection.Success {
				t.Error("b_student succeeded despite a failing credential")
			}
			if !sub.Collection.LastSuccess.Equal(firstSuccess["b_student"]) {
				t.Errorf("b_student last success changed from %s to %s",
					firstSuccess["b_student"], sub.Collection.LastSuccess)
			}
			if len(sub.Quotas) != 5 {
				t.Errorf("b_student last valid quotas lost: %+v", sub.Quotas)
			}
			if sub.MonthlyCostUSD == nil || *sub.MonthlyCostUSD != 19 {
				t.Errorf("b_student last valid price lost: %v", sub.MonthlyCostUSD)
			}
			if !strings.Contains(sub.Collection.Error, "[redacted]") {
				t.Errorf("b_student error not sanitized: %q", sub.Collection.Error)
			}
		case "b_personal", "r_personal":
			if !sub.Collection.Success {
				t.Errorf("%s was affected by another credential's failure: %+v", sub.Name, sub.Collection)
			}
		}
	}
	if strings.Contains(logBuf.String(), "crsr_b_student_secret") {
		t.Errorf("logs leak a Cursor credential: %s", logBuf.String())
	}

	values, text = gatherCursorMetrics(t, coll.Snapshot)
	if got := values["ai_usage_collection_success{provider=cursor,subscription=b_student}"]; got != 0 {
		t.Errorf("b_student collection_success = %v, want 0", got)
	}
	if got := values["ai_usage_collection_partial{provider=cursor,subscription=b_student}"]; got != 0 {
		t.Errorf("b_student collection_partial = %v, want 0 on hard failure", got)
	}
	if _, ok := values["ai_usage_collection_last_success_timestamp_seconds{provider=cursor,subscription=b_student}"]; !ok {
		t.Error("b_student last success series disappeared")
	}
	for _, label := range []string{"b_personal", "r_personal"} {
		if got := values["ai_usage_collection_success{provider=cursor,subscription="+label+"}"]; got != 1 {
			t.Errorf("%s collection_success = %v, want 1", label, got)
		}
	}
	for _, quota := range []struct{ name, window string }{
		{"total", "monthly"},
		{"included", "monthly"},
		{"api", "monthly"},
		{"on_demand", "monthly"},
		{"grok", "weekly"},
	} {
		key := "ai_usage_quota_used_ratio{provider=cursor,quota=" + quota.name +
			",subscription=b_student,window=" + quota.window + "}"
		if _, ok := values[key]; !ok {
			t.Errorf("b_student last valid quota series disappeared: %s", key)
		}
	}
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("metrics leak a Cursor credential: %s", text)
		}
	}
}
