package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/BaptTF/ai-usage-exporter/internal/collector"
	"github.com/BaptTF/ai-usage-exporter/internal/metrics"
	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

type flakyProvider struct {
	calls atomic.Int32
}

func (p *flakyProvider) Name() string            { return "fake" }
func (p *flakyProvider) Subscriptions() []string { return []string{"main"} }
func (p *flakyProvider) Collect(ctx context.Context) []provider.Result {
	if p.calls.Add(1) == 1 {
		sev := 0
		return []provider.Result{{
			Subscription: "main",
			Data: snapshot.Subscription{Quotas: []snapshot.Quota{{
				Window: "monthly", Quota: "total", UsedRatio: 0.5, Severity: &sev,
			}}},
			Duration: 10 * time.Millisecond,
		}}
	}
	return []provider.Result{{
		Subscription: "main",
		Err:          errUpstream{},
		Duration:     20 * time.Millisecond,
	}}
}

type errUpstream struct{}

func (errUpstream) Error() string { return "upstream unavailable" }

func testHandler(t *testing.T, ready, shutting bool) http.Handler {
	t.Helper()
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_metric", Help: "test"})
	g.Set(1)
	reg.MustRegister(g)
	return NewHandler(reg, func() bool { return ready }, func() bool { return shutting })
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthAndReadinessRoutes(t *testing.T) {
	h := testHandler(t, true, false)

	rec := do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("healthz = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ready"`) {
		t.Errorf("readyz = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "test_metric") || !strings.Contains(rec.Body.String(), "# HELP") {
		t.Errorf("metrics body = %s", rec.Body.String())
	}

	// Parse the exposition with the Prometheus text parser: this catches
	// malformed lines, duplicate series and invalid label syntax.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatalf("invalid Prometheus exposition: %v\n%s", err, rec.Body.String())
	}
	if _, ok := families["test_metric"]; !ok {
		t.Error("test_metric missing after parsing")
	}
}

func TestNotReadyButHealthy(t *testing.T) {
	h := testHandler(t, false, false)
	rec := do(t, h, http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "starting") {
		t.Errorf("readyz = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("healthz = %d", rec.Code)
	}
}

func TestShuttingDown(t *testing.T) {
	h := testHandler(t, true, true)
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := do(t, h, http.MethodGet, path)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting_down") {
			t.Errorf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestUnknownRouteAndMethods(t *testing.T) {
	h := testHandler(t, true, false)
	rec := do(t, h, http.MethodGet, "/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown route = %d", rec.Code)
	}
	rec = do(t, h, http.MethodPost, "/metrics")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestProviderFailureDoesNotGateReadinessOrScrape(t *testing.T) {
	snap := &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider: "opencode", Name: "main",
		Collection: snapshot.Collection{Success: false, Error: "upstream down"},
	}}}
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.New(func() *snapshot.Snapshot { return snap }))
	h := NewHandler(reg, func() bool { return true }, func() bool { return false })

	rec := do(t, h, http.MethodGet, "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("readyz gated by provider failure: %d", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Errorf("metrics status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `ai_usage_collection_success{provider="opencode",subscription="main"} 0`) {
		t.Errorf("metrics missing collection_success=0:\n%s", rec.Body.String())
	}
}

func TestHTTPMetricsServeLastValidDataAfterFailure(t *testing.T) {
	p := &flakyProvider{}
	c, err := collector.New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.New(c.Snapshot))

	first := c.CollectOnce(context.Background())
	lastSuccess := first.Subscriptions[0].Collection.LastSuccess
	h := NewHandler(reg, func() bool { return true }, func() bool { return false })

	rec := do(t, h, http.MethodGet, "/metrics")
	if !strings.Contains(rec.Body.String(), `ai_usage_quota_used_ratio{provider="fake",quota="total",subscription="main",window="monthly"} 0.5`) {
		t.Fatalf("first scrape missing quota:\n%s", rec.Body.String())
	}

	// Second collection fails: the scrape must keep the last valid quota and
	// flip collection_success to 0, with an unchanged last_success.
	c.CollectOnce(context.Background())
	rec = do(t, h, http.MethodGet, "/metrics")
	body := rec.Body.String()
	if !strings.Contains(body, `ai_usage_quota_used_ratio{provider="fake",quota="total",subscription="main",window="monthly"} 0.5`) {
		t.Errorf("stale quota disappeared:\n%s", body)
	}
	if !strings.Contains(body, `ai_usage_collection_success{provider="fake",subscription="main"} 0`) {
		t.Errorf("collection_success not 0:\n%s", body)
	}
	wantTS := `ai_usage_collection_last_success_timestamp_seconds{provider="fake",subscription="main"} ` +
		strconv.FormatFloat(float64(lastSuccess.Unix()), 'g', -1, 64)
	if !strings.Contains(body, wantTS) {
		t.Errorf("last_success changed or missing:\nwant %s\n%s", wantTS, body)
	}
}

func TestMetricsNeverExposeSecretsOrErrorMessages(t *testing.T) {
	const secret = "sk-live-super-secret"
	snap := &snapshot.Snapshot{Subscriptions: []snapshot.Subscription{{
		Provider: "opencode", Name: "main",
		Collection: snapshot.Collection{Success: false, Error: "auth failed for " + secret},
	}}}
	reg := prometheus.NewRegistry()
	reg.MustRegister(metrics.New(func() *snapshot.Snapshot { return snap }))
	h := NewHandler(reg, func() bool { return true }, func() bool { return false })

	rec := do(t, h, http.MethodGet, "/metrics")
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Errorf("metrics leak the secret:\n%s", body)
	}
	if strings.Contains(body, "auth failed") {
		t.Errorf("metrics contain the error message:\n%s", body)
	}
	if strings.Contains(body, "NaN") || strings.Contains(body, "Inf") {
		t.Errorf("metrics contain NaN/Inf:\n%s", body)
	}
}

func TestRunStartsReadyAndShutsDownGracefully(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_metric", Help: "test"}))
	srv := New("127.0.0.1:0", reg, log.New(io.Discard, "", 0))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	var addr string
	for i := 0; i < 100; i++ {
		addr = srv.BoundAddr()
		if addr != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		cancel()
		t.Fatal("server never bound a listener")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/readyz")
		if err == nil {
			ok := resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ok {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
