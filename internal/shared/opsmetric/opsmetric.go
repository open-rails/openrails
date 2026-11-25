// Package opsmetric emits operational measurements as structured log lines
// carrying a stable `metric` name for log-based alerting. The names are a
// contract with alert rules, so they live here, not at call sites.
package opsmetric

import (
	"context"
	"log/slog"
)

const (
	// MetricRosterRatio is emitted on every absence-capable reconcile pass, not
	// only when the breaker trips, so the ratio can be trended.
	MetricRosterRatio = "reconcile.roster_ratio"

	// MetricCancellationsPerPass is the planned-vs-allowed cancellation count
	// for one merchant's pass, with whether the cap held it.
	MetricCancellationsPerPass = "reconcile.cancellations_per_pass"

	// MetricRetentionSweep is one retention pass: how many merchants had due
	// work, how many rows each sweep removed, and how long it took.
	MetricRetentionSweep = "retention.sweep"

	// MetricAttributionDropped is one attribution field a checkout left out
	// (too long, unprintable, or carrying personal data), by field and reason.
	MetricAttributionDropped = "checkout.attribution_dropped"
)

// Emit writes one metric line. Info level: these are measurements, not
// incidents — the tripped/capped cases keep their own Error/Warn lines.
func Emit(ctx context.Context, logger *slog.Logger, name string, fields []any) {
	entry := logger
	if len(fields) > 0 {
		entry = entry.With(fields...)
	}
	entry.InfoContext(ctx, name, "metric", name)
}
