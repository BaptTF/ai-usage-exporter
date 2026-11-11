package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestProviderSubscriptions(t *testing.T) {
	p := NewProvider(nil, []KeyConfig{{EnvVar: "A", Label: "main"}, {EnvVar: "B", Label: "r"}})
	if p.Name() != "opencode" {
		t.Errorf("Name = %q", p.Name())
	}
	subs := p.Subscriptions()
	if len(subs) != 2 || subs[0] != "main" || subs[1] != "r" {
		t.Errorf("Subscriptions = %v", subs)
	}
}

func TestProviderCollectOneKeyFailsOthersSucceed(t *testing.T) {
	srv := newUsageServer(t, map[string]int{
		"sk-good": 200,
		"sk-bad":  401,
	})
	defer srv.Close()

	p := NewProvider(NewClientWithURL(time.Second, srv.URL), []KeyConfig{
		{Label: "good", Key: "sk-good"},
		{Label: "bad", Key: "sk-bad"},
	})
	results := p.Collect(context.Background())
	if len(results) != 2 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Subscription != "good" || results[0].Err != nil {
		t.Errorf("good result = %+v", results[0])
	}
	if results[0].Data.Provider != "opencode" || len(results[0].Data.Quotas) != 3 {
		t.Errorf("good data = %+v", results[0].Data)
	}
	if results[1].Subscription != "bad" || results[1].Err == nil {
		t.Errorf("bad result = %+v", results[1])
	}
}

func newUsageServer(t *testing.T, statusByKey map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		status, ok := statusByKey[key]
		if !ok {
			status = 500
		}
		w.WriteHeader(status)
		if status == 200 {
			_, _ = w.Write([]byte(usageBody))
		} else {
			_, _ = w.Write([]byte("nope"))
		}
	}))
}
