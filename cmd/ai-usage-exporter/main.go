// Command ai-usage-exporter polls multiple AI subscriptions and exposes their
// quotas as Prometheus metrics. It has no UI and never accepts Prometheus
// traffic; scraping is strictly pull-based over /metrics.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BaptTF/ai-usage-exporter/internal/collector"
	"github.com/BaptTF/ai-usage-exporter/internal/config"
	"github.com/BaptTF/ai-usage-exporter/internal/cursor"
	"github.com/BaptTF/ai-usage-exporter/internal/devin"
	"github.com/BaptTF/ai-usage-exporter/internal/metrics"
	"github.com/BaptTF/ai-usage-exporter/internal/opencode"
	"github.com/BaptTF/ai-usage-exporter/internal/provider"
	"github.com/BaptTF/ai-usage-exporter/internal/server"
)

func main() {
	logger := log.New(os.Stdout, "ai-usage-exporter: ", log.LstdFlags|log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("configuration error: %v", err)
	}

	providers := buildProviders(cfg)
	coll, err := collector.New(providers, cfg.Secrets(), logger)
	if err != nil {
		logger.Fatalf("collector error: %v", err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(metrics.New(coll.Snapshot))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		coll.Run(ctx, cfg.PollInterval)
	}()

	logProviderSummary(logger, cfg, providers)

	srv := server.New(":"+strconv.Itoa(cfg.Port), registry, logger)
	if err := srv.Run(ctx); err != nil {
		logger.Printf("server error: %v", err)
		stop()
		<-pollDone
		os.Exit(1)
	}
	<-pollDone
	logger.Printf("stopped")
}

func buildProviders(cfg config.Config) []provider.Provider {
	var out []provider.Provider
	if cfg.OpenCodeEnabled() {
		out = append(out, opencode.NewProvider(opencode.NewClient(cfg.RequestTimeout), cfg.OpenCodeKeys))
	}
	for _, key := range cfg.CursorKeys {
		out = append(out, cursor.NewProvider(cursor.NewClient(key.ClientConfig(cfg.RequestTimeout)), key.Label))
	}
	for _, key := range cfg.DevinKeys {
		client := devin.NewClient(
			key.ClientConfig(cfg.RequestTimeout, cfg.DevinResetDay, cfg.DevinSeatURL),
		)
		out = append(out, devin.NewProvider(client, key.Label))
	}
	return out
}

func logProviderSummary(logger *log.Logger, cfg config.Config, providers []provider.Provider) {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name())
	}
	logger.Printf("starting port=%d poll_interval=%s request_timeout=%s providers=%v",
		cfg.Port, cfg.PollInterval, cfg.RequestTimeout, names)
	if cfg.OpenCodeEnabled() {
		logger.Printf("opencode configured with %d subscription(s)", len(cfg.OpenCodeKeys))
	}
	for _, key := range cfg.CursorKeys {
		// Only the subscription, auth mode and variable name; the credential
		// never appears.
		logger.Printf("cursor configured subscription=%s (%s)", key.Label, key.AuthModeLabel())
	}
	for _, key := range cfg.DevinKeys {
		org := "resolved from /v3/self"
		if key.OrgID != "" {
			org = "org configured"
		}
		logger.Printf("devin configured subscription=%s (%s, reset day %d)", key.Label, org, cfg.DevinResetDay)
	}
	if len(providers) == 0 {
		// Defensive: config validation already rejects this.
		logger.Fatalf("no provider configured")
	}
}
