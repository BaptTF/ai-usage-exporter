// Package provider defines the small interface between a data source and the
// collection loop. Providers collect independently; one failing provider must
// never prevent another from producing data.
package provider

import (
	"context"
	"time"

	"github.com/BaptTF/ai-usage-exporter/internal/snapshot"
)

// Result is the outcome of collecting one subscription.
//
// Err non-nil means the primary source failed; Data is then ignored and the
// collector keeps the previous valid data. Partial marks a successful primary
// collection whose secondary source failed. Diagnostic carries the secondary
// error text; the collector sanitizes it before logging. It is never exposed
// as a metric label.
type Result struct {
	Subscription string
	Data         snapshot.Subscription
	Err          error
	Partial      bool
	Diagnostic   string
	Duration     time.Duration
}

// Provider fetches every subscription it owns. Subscriptions returns the
// stable, bounded set of subscription names so the collector can pre-seed
// state and expose collection_success=0 before the first poll completes.
type Provider interface {
	Name() string
	Subscriptions() []string
	Collect(ctx context.Context) []Result
}
