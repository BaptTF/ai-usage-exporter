package collector

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

type fakeProvider struct {
	name string
	subs []string
	fn   func(ctx context.Context) []provider.Result
}

func (f *fakeProvider) Name() string            { return f.name }
func (f *fakeProvider) Subscriptions() []string { return f.subs }
func (f *fakeProvider) Collect(ctx context.Context) []provider.Result {
	return f.fn(ctx)
}

func dataFor(providerName, sub string, ratio float64) snapshot.Subscription {
	sev := 1
	return snapshot.Subscription{
		Provider: providerName,
		Name:     sub,
		Quotas: []snapshot.Quota{{
			Window: "monthly", Quota: "total", UsedRatio: ratio, Severity: &sev,
		}},
	}
}

func TestNewSeedsCollectionMetrics(t *testing.T) {
	p := &fakeProvider{name: "fake", subs: []string{"a", "b"}}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := c.Snapshot()
	if len(snap.Subscriptions) != 2 {
		t.Fatalf("subscriptions = %+v", snap.Subscriptions)
	}
	for _, sub := range snap.Subscriptions {
		if sub.Collection.Success || !sub.Collection.LastSuccess.IsZero() {
			t.Errorf("unexpected collection state: %+v", sub.Collection)
		}
		if sub.Collection.Duration != 0 {
			t.Errorf("duration = %v", sub.Collection.Duration)
		}
	}
}

func TestNewRejectsDuplicateSubscriptions(t *testing.T) {
	p := &fakeProvider{name: "fake", subs: []string{"same", "same"}}
	if _, err := New([]provider.Provider{p}, nil, nil); err == nil {
		t.Fatal("expected duplicate subscription error")
	}
}

func TestLastValidDataSurvivesFailure(t *testing.T) {
	var calls atomic.Int32
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			if calls.Add(1) == 1 {
				return []provider.Result{{
					Subscription: "main",
					Data:         dataFor("fake", "main", 0.42),
					Duration:     10 * time.Millisecond,
				}}
			}
			return []provider.Result{{
				Subscription: "main",
				Err:          &testError{"boom with secret tok-super-secret"},
				Duration:     7 * time.Millisecond,
			}}
		},
	}
	c, err := New([]provider.Provider{p}, []string{"tok-super-secret"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	var ticks atomic.Int64
	c.SetClock(func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * time.Second) })

	first := c.CollectOnce(context.Background())
	firstSub := first.Subscriptions[0]
	if !firstSub.Collection.Success || firstSub.Collection.LastSuccess.IsZero() {
		t.Fatalf("first collection = %+v", firstSub.Collection)
	}
	lastSuccess := firstSub.Collection.LastSuccess

	second := c.CollectOnce(context.Background())
	secondSub := second.Subscriptions[0]
	if secondSub.Collection.Success {
		t.Error("second collection Success = true, want false")
	}
	if !secondSub.Collection.LastSuccess.Equal(lastSuccess) {
		t.Errorf("LastSuccess = %s, want unchanged %s", secondSub.Collection.LastSuccess, lastSuccess)
	}
	if len(secondSub.Quotas) != 1 || secondSub.Quotas[0].UsedRatio != 0.42 {
		t.Errorf("last valid data lost: %+v", secondSub.Quotas)
	}
	if secondSub.Collection.Duration != 7*time.Millisecond {
		t.Errorf("duration = %v, want 7ms", secondSub.Collection.Duration)
	}
	if strings.Contains(secondSub.Collection.Error, "tok-super-secret") {
		t.Errorf("snapshot error leaks the secret: %s", secondSub.Collection.Error)
	}
	if !strings.Contains(secondSub.Collection.Error, "[redacted]") {
		t.Errorf("snapshot error not redacted: %s", secondSub.Collection.Error)
	}
	if strings.Contains(logBuf.String(), "tok-super-secret") {
		t.Errorf("logs leak the secret: %s", logBuf.String())
	}
}

func TestProvidersAreCollectedInParallel(t *testing.T) {
	// Each provider sends one token then waits to receive the peer's token.
	// If CollectOnce ran providers sequentially, the first would wait until the
	// timeout instead of succeeding.
	started := make(chan struct{}, 2)
	newProvider := func(name string) provider.Provider {
		return &fakeProvider{
			name: name,
			subs: []string{"main"},
			fn: func(ctx context.Context) []provider.Result {
				started <- struct{}{}
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					return []provider.Result{{Subscription: "main", Err: errTimeout{}}}
				}
				return []provider.Result{{
					Subscription: "main",
					Data:         dataFor(name, "main", 0.5),
				}}
			},
		}
	}

	c, err := New([]provider.Provider{newProvider("one"), newProvider("two")}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := c.CollectOnce(context.Background())
	if len(snap.Subscriptions) != 2 {
		t.Fatalf("subscriptions = %+v", snap.Subscriptions)
	}
	for _, sub := range snap.Subscriptions {
		if !sub.Collection.Success || sub.Collection.Error != "" {
			t.Errorf("sub = %+v", sub.Collection)
		}
	}
}

func TestPartialDiagnosticIsLoggedAndSanitized(t *testing.T) {
	var logBuf bytes.Buffer
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			return []provider.Result{{
				Subscription: "main",
				Data:         dataFor("fake", "main", 0.5),
				Partial:      true,
				Diagnostic:   "grok failed with token tok-super-secret",
			}}
		},
	}
	c, err := New([]provider.Provider{p}, []string{"tok-super-secret"}, log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	sub := c.CollectOnce(context.Background()).Subscriptions[0]
	if !sub.Collection.Success || !sub.Collection.Partial {
		t.Errorf("collection = %+v", sub.Collection)
	}
	if sub.Collection.Error != "" {
		t.Errorf("secondary diagnostic leaked into collection error: %q", sub.Collection.Error)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "collection partial") || !strings.Contains(logs, "diagnostic=") {
		t.Errorf("partial diagnostic not logged: %s", logs)
	}
	if strings.Contains(logs, "tok-super-secret") {
		t.Errorf("logs leak the secondary diagnostic secret: %s", logs)
	}
	if !strings.Contains(logs, "[redacted]") {
		t.Errorf("partial diagnostic was not redacted: %s", logs)
	}
}

func TestPartialWithoutDiagnosticStillLogs(t *testing.T) {
	var logBuf bytes.Buffer
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			return []provider.Result{{
				Subscription: "main",
				Data:         dataFor("fake", "main", 0.5),
				Partial:      true,
			}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	c.CollectOnce(context.Background())
	logs := logBuf.String()
	if !strings.Contains(logs, "collection partial") || !strings.Contains(logs, "without a diagnostic") {
		t.Errorf("partial without diagnostic not logged: %s", logs)
	}
}

func TestProviderPanicDoesNotStopOthers(t *testing.T) {
	panicking := &fakeProvider{
		name: "boom",
		subs: []string{"x", "y"},
		fn: func(ctx context.Context) []provider.Result {
			panic("panic with secret tok-secret")
		},
	}
	healthy := &fakeProvider{
		name: "ok",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			return []provider.Result{{Subscription: "main", Data: dataFor("ok", "main", 0.2)}}
		},
	}
	var logBuf bytes.Buffer
	c, err := New([]provider.Provider{panicking, healthy}, []string{"tok-secret"}, log.New(&logBuf, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	snap := c.CollectOnce(context.Background())
	for _, sub := range snap.Subscriptions {
		switch sub.Provider {
		case "boom":
			if sub.Collection.Success || sub.Collection.Error != "provider panicked" {
				t.Errorf("panicking provider = %+v", sub.Collection)
			}
		case "ok":
			if !sub.Collection.Success {
				t.Errorf("healthy provider was affected: %+v", sub.Collection)
			}
		}
	}
	if strings.Contains(logBuf.String(), "tok-secret") {
		t.Errorf("logs leak the panic value: %s", logBuf.String())
	}
}

func TestMissingSubscriptionResultIsFailure(t *testing.T) {
	// The provider reports success for "a" but silently omits "b". The old
	// success=true for "b" must not survive.
	var calls atomic.Int32
	p := &fakeProvider{
		name: "fake",
		subs: []string{"a", "b"},
		fn: func(ctx context.Context) []provider.Result {
			if calls.Add(1) == 1 {
				return []provider.Result{
					{Subscription: "a", Data: dataFor("fake", "a", 0.1)},
					{Subscription: "b", Data: dataFor("fake", "b", 0.2)},
				}
			}
			return []provider.Result{{Subscription: "a", Data: dataFor("fake", "a", 0.1)}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.CollectOnce(context.Background())
	snap := c.CollectOnce(context.Background())
	for _, sub := range snap.Subscriptions {
		if sub.Name == "b" {
			if sub.Collection.Success {
				t.Error("omitted subscription stayed success=true")
			}
			if sub.Collection.Error != "provider returned no result" {
				t.Errorf("error = %q", sub.Collection.Error)
			}
			if len(sub.Quotas) != 1 {
				t.Errorf("last valid data lost: %+v", sub.Quotas)
			}
		}
	}
}

func TestUnknownResultIgnored(t *testing.T) {
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			return []provider.Result{
				{Subscription: "main", Data: dataFor("fake", "main", 0.2)},
				{Subscription: "unexpected", Data: dataFor("fake", "unexpected", 0.9)},
			}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := c.CollectOnce(context.Background())
	if len(snap.Subscriptions) != 1 {
		t.Fatalf("subscriptions = %+v", snap.Subscriptions)
	}
}

func TestRunDoesNotCollectWhenAlreadyCancelled(t *testing.T) {
	var calls atomic.Int32
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			calls.Add(1)
			return []provider.Result{{Subscription: "main", Data: dataFor("fake", "main", 0.1)}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx, time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("provider called %d times after cancellation, want 0", calls.Load())
	}
}

func TestCollectOnceCancelledDoesNotCallProviders(t *testing.T) {
	var calls atomic.Int32
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			calls.Add(1)
			return []provider.Result{{Subscription: "main", Data: dataFor("fake", "main", 0.1)}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.CollectOnce(ctx)
	if calls.Load() != 0 {
		t.Errorf("provider called %d times with a cancelled context, want 0", calls.Load())
	}
}

func TestRunNeverStartsAnotherPollAfterCancellation(t *testing.T) {
	// Cancellation happens during the first collection. The ticker interval is
	// tiny, so ticker.C is likely ready at the same time as ctx.Done; a new
	// poll must still never start.
	for i := 0; i < 25; i++ {
		var calls atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		p := &fakeProvider{
			name: "fake",
			subs: []string{"main"},
			fn: func(ctx context.Context) []provider.Result {
				if calls.Add(1) == 1 {
					cancel()
				}
				return []provider.Result{{Subscription: "main", Data: dataFor("fake", "main", 0.1)}}
			},
		}
		c, err := New([]provider.Provider{p}, nil, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		c.Run(ctx, time.Nanosecond)
		cancel()
		if got := calls.Load(); got != 1 {
			t.Fatalf("iteration %d: provider called %d times, want exactly 1", i, got)
		}
	}
}

func TestRunReturnsWhenBlockedProviderHonoursCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return []provider.Result{{Subscription: "main", Err: ctx.Err()}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, time.Hour)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("provider never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run deadlocked after cancellation")
	}
}

func TestConcurrentSnapshotReads(t *testing.T) {
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			return []provider.Result{{Subscription: "main", Data: dataFor("fake", "main", 0.3)}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			c.CollectOnce(context.Background())
		}
	}()
	for i := 0; i < 200; i++ {
		if snap := c.Snapshot(); snap == nil {
			t.Error("nil snapshot")
		}
	}
	wg.Wait()
}

func TestNormalizeForcesConfiguredLabels(t *testing.T) {
	p := &fakeProvider{
		name: "fake",
		subs: []string{"main"},
		fn: func(ctx context.Context) []provider.Result {
			return []provider.Result{{
				Subscription: "main",
				Data:         snapshot.Subscription{Provider: "spoofed", Name: "other"},
			}}
		},
	}
	c, err := New([]provider.Provider{p}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := c.CollectOnce(context.Background()).Subscriptions[0]
	if sub.Provider != "fake" || sub.Name != "main" {
		t.Errorf("labels = %s/%s", sub.Provider, sub.Name)
	}
}

func TestSanitize(t *testing.T) {
	got := sanitize("token abc123 rejected", []string{"abc123", " "})
	if strings.Contains(got, "abc123") || !strings.Contains(got, "[redacted]") {
		t.Errorf("sanitize = %q", got)
	}

	// Derived credentials are not in the configured secret list; generic
	// credential shapes still must not reach logs.
	derived := "Authorization: Bearer eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyIn0.sig and crsr_abcdefghij"
	got = sanitize(derived, nil)
	for _, leak := range []string{"eyJhbGciOiJub25lIn0", "crsr_abcdefghij"} {
		if strings.Contains(got, leak) {
			t.Errorf("sanitize left %q in %q", leak, got)
		}
	}
	long := strings.Repeat("x", maxErrorLength+50)
	if got := sanitize(long, nil); len(got) != maxErrorLength+3 {
		t.Errorf("len(sanitize(long)) = %d, want %d", len(got), maxErrorLength+3)
	}
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }

type errTimeout struct{}

func (errTimeout) Error() string { return "timeout" }
