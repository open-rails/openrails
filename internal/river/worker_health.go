package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #689: per-worker health bookkeeping. One middleware wraps EVERY registered
// worker (zero per-worker code) and upserts openrails.worker_health per job
// completion; a periodic checker raises durable repair alerts when a kind has
// never succeeded, keeps failing, or went stale past its declared cadence.

const KindWorkerHealthCheck = "openrails.worker_health_check"

// maxWorkerHealthErrorLen bounds last_error (runes, so truncation never splits
// a UTF-8 sequence — Postgres rejects invalid text).
const maxWorkerHealthErrorLen = 2000

type WorkerHealthCheckArgs struct{}

func (WorkerHealthCheckArgs) Kind() string { return KindWorkerHealthCheck }

// WorkerRegistrations captures every registered worker kind and, for periodic
// kinds, the expected cadence — recorded at registration time so the checker
// knows what "healthy" means without per-worker code.
type WorkerRegistrations struct {
	mu      sync.Mutex
	periods map[string]time.Duration // 0 = on-demand (no cadence)
}

func NewWorkerRegistrations() *WorkerRegistrations {
	return &WorkerRegistrations{periods: make(map[string]time.Duration)}
}

// NoteKind records a registered worker kind (no cadence yet).
func (r *WorkerRegistrations) NoteKind(kind string) {
	if r == nil || kind == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.periods[kind]; !ok {
		r.periods[kind] = 0
	}
}

// NotePeriod records a periodic cadence for kind, keeping the SHORTEST declared
// interval (a kind scheduled hourly + monthly is expected hourly).
func (r *WorkerRegistrations) NotePeriod(kind string, period time.Duration) {
	if r == nil || kind == "" || period <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.periods[kind]; !ok || cur == 0 || period < cur {
		r.periods[kind] = period
	}
}

// Snapshot returns a copy of kind -> expected period.
func (r *WorkerRegistrations) Snapshot() map[string]time.Duration {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]time.Duration, len(r.periods))
	for k, v := range r.periods {
		out[k] = v
	}
	return out
}

// WorkerHealthMiddleware is a rivertype.WorkerMiddleware installed once on the
// client: after every worked job it upserts the kind's health row. Bookkeeping
// failures are logged, never surfaced as job errors.
type WorkerHealthMiddleware struct {
	river.MiddlewareDefaults
	DB    *db.DB
	Clock clockwork.Clock
}

func NewWorkerHealthMiddleware(database *db.DB) *WorkerHealthMiddleware {
	return &WorkerHealthMiddleware{DB: database}
}

func (m *WorkerHealthMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	err := doInner(ctx)
	m.record(ctx, job.Kind, err)
	return err
}

func (m *WorkerHealthMiddleware) record(ctx context.Context, kind string, workErr error) {
	if m == nil || m.DB == nil || kind == "" {
		return
	}
	// A snooze is a deliberate reschedule, not an outcome — record nothing.
	if workErr != nil && errors.Is(workErr, &rivertype.JobSnoozeError{}) {
		return
	}
	now := time.Now().UTC()
	if m.Clock != nil {
		now = m.Clock.Now().UTC()
	}
	// Detached ctx: a cancelled/timed-out job must still get its failure recorded.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	if workErr == nil {
		err = m.DB.Gen(rctx).RecordWorkerSuccess(rctx, gen.RecordWorkerSuccessParams{WorkerKind: kind, Now: now})
	} else {
		msg := truncateRunes(workErr.Error(), maxWorkerHealthErrorLen)
		err = m.DB.Gen(rctx).RecordWorkerFailure(rctx, gen.RecordWorkerFailureParams{WorkerKind: kind, Now: now, LastError: &msg})
	}
	if err != nil {
		log.WithError(err).WithField("worker_kind", kind).Warn("worker health: bookkeeping upsert failed")
	}
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// WorkerHealthCheckWorker is the periodic alert evaluator: it seeds a health
// row for every registered kind (so never-run workers are visible from day
// one), evaluates the alert rules and routes trips to the existing repair-alert
// channel (notification_queue system alerts, per active merchant — the same
// admin surface the ledger reconcilers use). Its own runs heartbeat through the
// same table, so a wedged checker is at least visible as a stale row.
type WorkerHealthCheckWorker struct {
	river.WorkerDefaults[WorkerHealthCheckArgs]
	DB                  *db.DB
	Clock               clockwork.Clock
	Registrations       *WorkerRegistrations
	NotificationService *subscriptions.NotificationService

	// Tunables; zero values take the defaults below.
	FailureThreshold int           // consecutive failures before alerting (default 3)
	StaleMultiplier  int           // k in "no success within k x expected period" (default 3)
	MinStale         time.Duration // floor for the staleness threshold (default 30m)
	ReAlertEvery     time.Duration // re-alert pacing while a kind stays unhealthy (default 24h)
}

func (WorkerHealthCheckWorker) Kind() string { return KindWorkerHealthCheck }

func (w *WorkerHealthCheckWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *WorkerHealthCheckWorker) failureThreshold() int {
	if w.FailureThreshold > 0 {
		return w.FailureThreshold
	}
	return 3
}

func (w *WorkerHealthCheckWorker) staleMultiplier() int {
	if w.StaleMultiplier > 0 {
		return w.StaleMultiplier
	}
	return 3
}

func (w *WorkerHealthCheckWorker) minStale() time.Duration {
	if w.MinStale > 0 {
		return w.MinStale
	}
	return 30 * time.Minute
}

func (w *WorkerHealthCheckWorker) reAlertEvery() time.Duration {
	if w.ReAlertEvery > 0 {
		return w.ReAlertEvery
	}
	return 24 * time.Hour
}

func (w *WorkerHealthCheckWorker) Work(ctx context.Context, _ *river.Job[WorkerHealthCheckArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("worker health check: DB is required")
	}
	now := w.now()
	q := w.DB.Gen(ctx)

	// Seed a row per registered kind so "expected N runs, got 0" is a visible
	// row, not an absence. Idempotent; refreshes the declared cadence.
	registered := w.Registrations.Snapshot()
	for kind, period := range registered {
		var secs *int64
		if period > 0 {
			s := int64(period / time.Second)
			secs = &s
		}
		if err := q.SeedWorkerHealth(ctx, gen.SeedWorkerHealthParams{WorkerKind: kind, ExpectedPeriodSeconds: secs}); err != nil {
			return fmt.Errorf("worker health check: seed %s: %w", kind, err)
		}
	}

	rows, err := q.ListWorkerHealth(ctx)
	if err != nil {
		return fmt.Errorf("worker health check: list: %w", err)
	}

	var alerted, failedAlerts int
	for _, row := range rows {
		// Rows for kinds not registered in THIS deploy (retired workers) stay
		// visible on the admin surface but never alert. A nil registration set
		// (manual/test invocation) evaluates everything.
		if registered != nil {
			if _, ok := registered[row.WorkerKind]; !ok {
				continue
			}
		}
		reason := evaluateWorkerHealth(row, now, w.failureThreshold(), w.staleMultiplier(), w.minStale())
		if reason == "" || !workerAlertDue(row, now, w.reAlertEvery()) {
			continue
		}
		if err := w.raiseAlert(ctx, row, reason, now); err != nil {
			failedAlerts++
			log.WithContext(ctx).WithError(err).WithField("worker_kind", row.WorkerKind).
				Error("worker health check: failed to raise repair alert")
			continue
		}
		alerted++
		if err := q.MarkWorkerHealthAlerted(ctx, gen.MarkWorkerHealthAlertedParams{WorkerKind: row.WorkerKind, Now: now}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("worker_kind", row.WorkerKind).
				Warn("worker health check: failed to mark alerted")
		}
	}
	if alerted > 0 || failedAlerts > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"alerted": alerted, "failed": failedAlerts}).
			Warn("worker health check: unhealthy workers")
	}

	// Explicit heartbeat: even without the middleware attached (external River
	// wiring), a completed evaluation is recorded.
	if err := q.RecordWorkerSuccess(ctx, gen.RecordWorkerSuccessParams{WorkerKind: KindWorkerHealthCheck, Now: now}); err != nil {
		log.WithContext(ctx).WithError(err).Warn("worker health check: heartbeat failed")
	}
	if failedAlerts > 0 {
		return fmt.Errorf("worker health check: %d repair alerts failed to record", failedAlerts)
	}
	return nil
}

// evaluateWorkerHealth returns "" when healthy, else the alert reason.
func evaluateWorkerHealth(row gen.OpenrailsWorkerHealth, now time.Time, failureThreshold, staleMultiplier int, minStale time.Duration) string {
	if int(row.ConsecutiveFailures) >= failureThreshold {
		return "consecutive_failures"
	}
	if row.ExpectedPeriodSeconds == nil || *row.ExpectedPeriodSeconds <= 0 {
		return "" // on-demand kind: no cadence to be late against
	}
	threshold := time.Duration(*row.ExpectedPeriodSeconds) * time.Second * time.Duration(staleMultiplier)
	if threshold < minStale {
		threshold = minStale
	}
	if row.LastSuccessAt == nil {
		if now.Sub(row.RegisteredAt) > threshold {
			return "never_succeeded"
		}
		return ""
	}
	if now.Sub(*row.LastSuccessAt) > threshold {
		return "stale"
	}
	return ""
}

// workerAlertDue dedupes: alert on first trip, on a fresh incident (a success
// happened since the last alert), or on the re-alert pacing while it persists.
func workerAlertDue(row gen.OpenrailsWorkerHealth, now time.Time, reAlertEvery time.Duration) bool {
	if row.LastAlertedAt == nil {
		return true
	}
	if row.LastSuccessAt != nil && row.LastSuccessAt.After(*row.LastAlertedAt) {
		return true
	}
	return now.Sub(*row.LastAlertedAt) >= reAlertEvery
}

// workerHealthAlertIdempotencyKey identifies one alerting incident. Retry-
// volatile health details are deliberately excluded so a partial merchant
// fan-out retries only the deliveries that did not already persist.
func workerHealthAlertIdempotencyKey(row gen.OpenrailsWorkerHealth, reason string) string {
	lastSuccessAt := ""
	if row.LastSuccessAt != nil {
		lastSuccessAt = row.LastSuccessAt.UTC().Format(time.RFC3339Nano)
	}
	lastAlertedAt := ""
	if row.LastAlertedAt != nil {
		lastAlertedAt = row.LastAlertedAt.UTC().Format(time.RFC3339Nano)
	}
	return uuidutil.DeterministicID(
		uuidutil.DeterministicNamespace,
		"worker_health_alert",
		row.WorkerKind,
		reason,
		row.RegisteredAt.UTC().Format(time.RFC3339Nano),
		lastSuccessAt,
		lastAlertedAt,
	).String()
}

// raiseAlert routes one unhealthy kind to the existing repair-alert channel.
// Worker health is operator-global but the channel is merchant-scoped, so the
// alert fans out to every active merchant (embedded deployments have exactly
// one) — a wedged worker stalls billing for all of them.
func (w *WorkerHealthCheckWorker) raiseAlert(ctx context.Context, row gen.OpenrailsWorkerHealth, reason string, now time.Time) error {
	merchantIDs, err := w.DB.Gen(ctx).ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list merchants: %w", err)
	}
	if len(merchantIDs) == 0 {
		return fmt.Errorf("no active merchants to receive the alert")
	}
	metadata := map[string]any{
		"worker_kind":          row.WorkerKind,
		"reason":               reason,
		"consecutive_failures": row.ConsecutiveFailures,
	}
	if row.ExpectedPeriodSeconds != nil {
		metadata["expected_period_seconds"] = *row.ExpectedPeriodSeconds
	}
	if row.LastSuccessAt != nil {
		metadata["last_success_at"] = row.LastSuccessAt.UTC().Format(time.RFC3339)
	}
	if row.LastErrorAt != nil {
		metadata["last_error_at"] = row.LastErrorAt.UTC().Format(time.RFC3339)
	}
	alertErr := fmt.Errorf("worker %s unhealthy: %s", row.WorkerKind, reason)
	if row.LastError != nil && *row.LastError != "" {
		alertErr = fmt.Errorf("worker %s unhealthy (%s): %s", row.WorkerKind, reason, *row.LastError)
	}
	alertErrors := make([]error, 0)
	idempotencyKey := workerHealthAlertIdempotencyKey(row, reason)
	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := w.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			return webhooks.RecordLedgerRepairAlert(ctx, w.NotificationService, w.DB, now, webhooks.LedgerRepairAlert{
				Provider:       "openrails",
				Operation:      "worker_health",
				IdempotencyKey: idempotencyKey,
				Err:            alertErr,
				Metadata:       metadata,
			})
		}); err != nil {
			alertErrors = append(alertErrors, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	if err := errors.Join(alertErrors...); err != nil {
		return fmt.Errorf("record repair alerts: %w", err)
	}
	return nil
}
