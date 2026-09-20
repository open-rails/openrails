// Package opsmetric emits operational measurements as structured log lines.
//
// OpenRails ships no Prometheus/OTel exporter, and internal/modules/metrics is
// the MERCHANT-facing analytics layer (SQL over business tables) — neither is a
// home for "how did this pass behave". The operator's telemetry substrate is the
// log stream, so an operational metric is a line carrying a stable `metric` name
// plus its fields, which log-based alerting can threshold on without a new
// dependency.
//
// The names live here rather than at the call sites: a metric an alert rule
// references is a contract, and a contract spelled out inline drifts.
package opsmetric

import (
	"context"

	log "github.com/sirupsen/logrus"
)

const (
	// MetricRosterRatio is emitted on EVERY absence-capable reconcile pass —
	// not only when the breaker trips. `tripped=false` with a ratio sliding
	// toward the threshold is the signal that matters; a metric that only
	// exists at the moment of failure cannot be trended (or#837).
	MetricRosterRatio = "reconcile.roster_ratio"

	// MetricCancellationsPerPass is the planned-vs-allowed cancellation count
	// for one merchant's pass, with whether the cap held it.
	MetricCancellationsPerPass = "reconcile.cancellations_per_pass"

	// MetricRetentionSweep is one retention pass: how many merchants had due
	// work, how many rows each sweep removed, and how long it took.
	MetricRetentionSweep = "retention.sweep"
)

// Emit writes one metric line. Info level: these are measurements, not
// incidents — the tripped/capped cases keep their own Error/Warn lines.
func Emit(ctx context.Context, name string, fields log.Fields) {
	entry := log.WithContext(ctx)
	if len(fields) > 0 {
		entry = entry.WithFields(fields)
	}
	entry.WithField("metric", name).Info(name)
}
