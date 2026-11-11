# ai-usage-exporter — agent guide

## Scope

Prometheus exporter for AI subscription quotas. No UI, no inbound Prometheus.
Business rules live in `internal/budget`; providers in `internal/{opencode,cursor,devin}`;
snapshot/collection in `internal/{snapshot,provider,collector}`; exposition in
`internal/metrics`; HTTP in `internal/server`.

## Invariants

- A provider failure must never erase the last valid data for that subscription.
  `ai_usage_collection_success` drops to 0 and last success stays unchanged.
- A secondary-source failure is `partial=1`, never a hard failure, and must not
  hide primary data (Grok for Cursor, seat quotas for Devin). Its diagnostic is
  carried in `provider.Result.Diagnostic`, sanitized by the collector before
  being logged, and never exposed as a metric or label.
- Never put credentials, tokens, error messages, or free-form API strings in
  metric labels. Labels are bounded enums (`window`, `quota`, `bucket`, `product`)
  or configuration-derived subscription names.
- `/readyz` depends on the HTTP server only, never on provider health.
- Every outbound HTTP request must use `http.NewRequestWithContext` and the
  caller's context, including auth/exchange/refresh, PlanInfo, Grok and the web
  fallback. `Collector.Run` must not start a new poll once its context is done.
  Shutdown residual is the 10 s HTTP `Shutdown` bound plus `REQUEST_TIMEOUT`
  only if a provider ignores context; the bundled clients honor it.
- Never add a quota/dry-day/severity value that cannot be derived. Rolling 5h has
  no elapsed or dry-day value and uses `AbsoluteLevel`. OpenCode monthly/weekly
  use `ComputeBudget`; if it is invalid, fall back to `AbsoluteLevel` and keep
  elapsed/dry absent.
- Cursor and Devin severities always use `AbsoluteLevel`, as in the legacy board;
  their pace metrics are informational and never override those thresholds.

## Verification

```sh
gofmt -w .
go vet ./...
go test -race -count=1 ./...
go build ./cmd/ai-usage-exporter
```

If `govulncheck` or `promtool` is available locally, run it; neither is a build
dependency.

## Secrets

Never log or expose credentials. `collector.sanitize` redacts configured secrets,
`Bearer …`, JWTs and common token prefixes before errors are stored or logged.
Startup logs may name variables and auth modes, never values.
