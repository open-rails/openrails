package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #895: the progress detector must NOT be a River job.
//
// It used to be — WorkerHealthCheckWorker was itself a River periodic job, so a
// stalled River stalled its own detector and could only report health in the
// cases where health was never in doubt. ProgressMonitor is a plain goroutine
// owned by OpenRails: it reads River's OWN `river_job` table plus the
// openrails.worker_health rows and decides whether the periodic fleet is
// progressing, without needing a job to run to find out.
//
// river_job is the primary signal deliberately. worker_health.last_success_at
// is written by WorkerHealthMiddleware, so a fleet running without that
// middleware reports never_succeeded for every kind forever (100% false alarms,
// measured 19/25 on cozy-art, 20/24 on tensorhub). river_job is written by
// River itself, so it stays truthful regardless of how the host wired things.

// Progress verdicts. Empty string means healthy.
const (
	// ProgressNotScheduling: River is not inserting OpenRails' periodic jobs at
	// all — the client was never started, the periodic jobs were never
	// registered, or the whole process is gone. This is the failure that used to
	// be invisible by construction.
	ProgressNotScheduling = "river_not_scheduling"
	// ProgressNotCompleting: rows ARE being inserted but nothing finalizes —
	// workers unregistered, the billing queue not configured on the host's
	// client, or every worker wedged.
	ProgressNotCompleting = "river_not_completing"
	// ProgressBacklog: work is queued past its due time and piling up.
	ProgressBacklog = "river_backlog"

	// Per-kind verdicts (evaluated against worker_health, secondary detail).
	ProgressConsecutiveFailures = "consecutive_failures"
	ProgressNeverSucceeded      = "never_succeeded"
	ProgressStale               = "stale"
)

// riverJobStat is one kind's watermark row from River's own table.
type riverJobStat struct {
	Kind            string
	LastEnqueuedAt  *time.Time
	LastCompletedAt *time.Time
	Overdue         int64
}

// KindProgress is one worker kind's verdict.
type KindProgress struct {
	Kind string
	// ExpectedPeriod is the declared cadence; 0 = on-demand (never stale).
	ExpectedPeriod time.Duration
	// LastEnqueuedAt/LastCompletedAt come from river_job (River's own writes).
	LastEnqueuedAt  *time.Time
	LastCompletedAt *time.Time
	// LastSuccessAt comes from openrails.worker_health (middleware bookkeeping).
	LastSuccessAt *time.Time
	Overdue       int64
	// Reason is "" when healthy.
	Reason string
}

// ProgressReport is one evaluation of the periodic fleet.
type ProgressReport struct {
	CheckedAt time.Time
	// Progressing is the headline: false means the cron fleet is not moving.
	Progressing bool
	// Reason is the fleet-level verdict ("" when progressing).
	Reason string
	// ShortestPeriod is the tightest declared cadence across registered kinds.
	ShortestPeriod time.Duration
	// FleetLastEnqueuedAt/FleetLastCompletedAt are the newest watermarks across
	// every OpenRails kind, read from river_job.
	FleetLastEnqueuedAt  *time.Time
	FleetLastCompletedAt *time.Time
	// Kinds carries per-kind detail, sorted by kind.
	Kinds []KindProgress
	// Unhealthy counts kinds with a non-empty Reason.
	Unhealthy int
}

// Err renders the report as an error when the fleet is not progressing, so a
// host health endpoint can `if err := e.CheckJobProgress(ctx); err != nil`.
func (r ProgressReport) Err() error {
	if r.Progressing {
		return nil
	}
	detail := "no completions observed"
	if r.FleetLastCompletedAt != nil {
		detail = "last completion " + r.FleetLastCompletedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Errorf("river periodic fleet is not progressing (%s): %s", r.Reason, detail)
}

// ProgressMonitor evaluates whether the River periodic fleet is progressing.
// It is NOT a River worker and never enqueues anything (#895): Check is a pure
// read, and Run is an ordinary ticker goroutine owned by the OpenRails runtime,
// so it keeps reporting while River itself is dead.
type ProgressMonitor struct {
	DB *db.DB
	// Pool reads River's own tables. River's schema is configurable and its
	// tables are not part of OpenRails' sqlc schema, so this is a raw read (see
	// internal/db/queries/EXEMPTIONS.md). Nil disables the river_job signal and
	// the monitor falls back to worker_health only.
	Pool        *pgxpool.Pool
	RiverSchema string
	Clock       clockwork.Clock
	// Registrations is the kind -> declared cadence set built at worker
	// registration; it is what "expected" means.
	Registrations       *WorkerRegistrations
	NotificationService *subscriptions.NotificationService

	// Tunables; zero values take the defaults below.
	FailureThreshold int           // consecutive failures before alerting (default 3)
	StaleMultiplier  int           // k in "no progress within k x expected period" (default 3)
	MinStale         time.Duration // floor for the staleness threshold (default 30m)
	ReAlertEvery     time.Duration // re-alert pacing while a kind stays unhealthy (default 24h)
	Interval         time.Duration // Run's tick (default 1m)

	// startedAt anchors the boot grace period: a freshly booted process has no
	// river_job rows yet and must not alarm before the fleet has had a chance to
	// schedule anything. Set on the first Check.
	startedAt time.Time
}

func (m *ProgressMonitor) now() time.Time {
	if m.Clock != nil {
		return m.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *ProgressMonitor) failureThreshold() int {
	if m.FailureThreshold > 0 {
		return m.FailureThreshold
	}
	return 3
}

func (m *ProgressMonitor) staleMultiplier() int {
	if m.StaleMultiplier > 0 {
		return m.StaleMultiplier
	}
	return defaultStaleMultiplier
}

func (m *ProgressMonitor) minStale() time.Duration {
	if m.MinStale > 0 {
		return m.MinStale
	}
	return defaultMinStale
}

// The ONE staleness rule of this package, shared by the fleet monitor (is this
// kind late?) and the per-job liveness middleware (is this running job
// wedged? — job_liveness.go, xs-007 row 31): no progress within k x the
// kind's declared cadence, floored so a tight cadence does not turn one slow
// provider round-trip into a stall. Neither number decides on its own; the
// cadence the worker declared at registration is what "late" is measured
// against.
const (
	defaultStaleMultiplier = 3
	defaultMinStale        = 30 * time.Minute
)

// staleThreshold applies the rule. period <= 0 is an on-demand kind: there is
// no cadence to multiply, so the floor is the whole tolerance.
func staleThreshold(period time.Duration, multiplier int, minStale time.Duration) time.Duration {
	threshold := period * time.Duration(multiplier)
	if threshold < minStale {
		threshold = minStale
	}
	return threshold
}

func (m *ProgressMonitor) reAlertEvery() time.Duration {
	if m.ReAlertEvery > 0 {
		return m.ReAlertEvery
	}
	return 24 * time.Hour
}

func (m *ProgressMonitor) interval() time.Duration {
	if m.Interval > 0 {
		return m.Interval
	}
	return time.Minute
}

// Run ticks Check + Alert until ctx is done. It is a plain goroutine on
// purpose: nothing about it goes through River, so River being wedged, never
// started, or never wired at all cannot suppress it.
func (m *ProgressMonitor) Run(ctx context.Context) {
	if m == nil || m.DB == nil {
		return
	}
	var ticker clockwork.Ticker
	if m.Clock != nil {
		ticker = m.Clock.NewTicker(m.interval())
	} else {
		ticker = clockwork.NewRealClock().NewTicker(m.interval())
	}
	defer ticker.Stop()
	log.WithField("interval", m.interval()).Info("river progress monitor started (#895: out-of-River detector)")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
			report, err := m.Check(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.WithError(err).Warn("river progress monitor: check failed")
				continue
			}
			if err := m.RaiseAlerts(ctx, report); err != nil && ctx.Err() == nil {
				log.WithError(err).Warn("river progress monitor: alerting failed")
			}
		}
	}
}

// Check evaluates the fleet. It seeds a health row per registered kind first,
// so a kind that has never run once is a visible row rather than an absence,
// then reads River's own river_job watermarks and the worker_health rows.
func (m *ProgressMonitor) Check(ctx context.Context) (ProgressReport, error) {
	if m == nil || m.DB == nil {
		return ProgressReport{}, fmt.Errorf("river progress: DB is required")
	}
	now := m.now()
	if m.startedAt.IsZero() {
		m.startedAt = now
	}
	q := m.DB.Gen(ctx)

	registered := m.Registrations.Snapshot()
	for kind, period := range registered {
		var secs *int64
		if period > 0 {
			s := int64(period / time.Second)
			secs = &s
		}
		if err := q.SeedWorkerHealth(ctx, gen.SeedWorkerHealthParams{WorkerKind: kind, ExpectedPeriodSeconds: secs}); err != nil {
			return ProgressReport{}, fmt.Errorf("river progress: seed %s: %w", kind, err)
		}
	}

	rows, err := q.ListWorkerHealth(ctx)
	if err != nil {
		return ProgressReport{}, fmt.Errorf("river progress: list worker health: %w", err)
	}

	kinds := make([]string, 0, len(registered))
	shortest := time.Duration(0)
	for kind, period := range registered {
		kinds = append(kinds, kind)
		if period > 0 && (shortest == 0 || period < shortest) {
			shortest = period
		}
	}
	sort.Strings(kinds)

	stats, err := m.readRiverJobStats(ctx, kinds)
	if err != nil {
		return ProgressReport{}, err
	}

	report := ProgressReport{CheckedAt: now, ShortestPeriod: shortest}
	byKind := make(map[string]gen.OpenrailsWorkerHealth, len(rows))
	for _, row := range rows {
		byKind[row.WorkerKind] = row
	}
	for _, kind := range kinds {
		stat := stats[kind]
		kp := KindProgress{
			Kind:            kind,
			ExpectedPeriod:  registered[kind],
			LastEnqueuedAt:  stat.LastEnqueuedAt,
			LastCompletedAt: stat.LastCompletedAt,
			Overdue:         stat.Overdue,
		}
		if row, ok := byKind[kind]; ok {
			kp.LastSuccessAt = row.LastSuccessAt
			kp.Reason = evaluateKindProgress(row, kp, now, m.failureThreshold(), m.staleMultiplier(), m.minStale())
		}
		if kp.Reason != "" {
			report.Unhealthy++
		}
		report.Kinds = append(report.Kinds, kp)
		report.FleetLastEnqueuedAt = laterOf(report.FleetLastEnqueuedAt, stat.LastEnqueuedAt)
		report.FleetLastCompletedAt = laterOf(report.FleetLastCompletedAt, stat.LastCompletedAt)
	}

	report.Reason = m.fleetVerdict(report, now)
	report.Progressing = report.Reason == ""
	return report, nil
}

// fleetVerdict answers Paul's question directly — "is the cron system
// progressing?" — from river_job watermarks alone, with a boot grace period so
// a process that just started does not alarm before its first tick could fire.
func (m *ProgressMonitor) fleetVerdict(report ProgressReport, now time.Time) string {
	if report.ShortestPeriod <= 0 {
		return "" // no periodic kinds registered: nothing to be late against
	}
	if m.Pool == nil {
		return "" // river_job unreadable by configuration; per-kind detail still applies
	}
	threshold := staleThreshold(report.ShortestPeriod, m.staleMultiplier(), m.minStale())
	// Boot grace: never alarm on a window the process was not alive for.
	if now.Sub(m.startedAt) < threshold {
		return ""
	}
	if report.FleetLastEnqueuedAt == nil || now.Sub(*report.FleetLastEnqueuedAt) > threshold {
		return ProgressNotScheduling
	}
	if report.FleetLastCompletedAt == nil || now.Sub(*report.FleetLastCompletedAt) > threshold {
		return ProgressNotCompleting
	}
	for _, kp := range report.Kinds {
		if kp.Overdue > 0 {
			return ProgressBacklog
		}
	}
	return ""
}

// readRiverJobStats reads River's OWN table. The kind list is bound as a
// parameter; only the schema is interpolated, and it is validated as a plain
// identifier first so it can never carry SQL.
func (m *ProgressMonitor) readRiverJobStats(ctx context.Context, kinds []string) (map[string]riverJobStat, error) {
	out := make(map[string]riverJobStat, len(kinds))
	if m.Pool == nil || len(kinds) == 0 {
		return out, nil
	}
	schema := strings.TrimSpace(m.RiverSchema)
	if schema == "" {
		schema = "public"
	}
	if !isPlainIdentifier(schema) {
		return nil, fmt.Errorf("river progress: refusing unsafe river schema %q", schema)
	}
	// Overdue = due in the past by more than one shortest-cadence beat and still
	// not finalized. now() is the DB clock deliberately: a skewed app clock must
	// not manufacture a backlog.
	sql := fmt.Sprintf(`
SELECT kind,
       max(created_at)                                      AS last_enqueued_at,
       max(finalized_at) FILTER (WHERE state = 'completed') AS last_completed_at,
       count(*) FILTER (WHERE state IN ('available', 'retryable')
                          AND scheduled_at < now() - $2::interval) AS overdue
FROM %s.river_job
WHERE kind = ANY($1::text[])
GROUP BY kind`, schema)

	overdueAfter := m.minStale()
	rows, err := m.Pool.Query(ctx, sql, kinds, overdueAfter.String())
	if err != nil {
		return nil, fmt.Errorf("river progress: read %s.river_job: %w", schema, err)
	}
	defer rows.Close()
	for rows.Next() {
		var s riverJobStat
		if err := rows.Scan(&s.Kind, &s.LastEnqueuedAt, &s.LastCompletedAt, &s.Overdue); err != nil {
			return nil, fmt.Errorf("river progress: scan river_job: %w", err)
		}
		out[s.Kind] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("river progress: read %s.river_job: %w", schema, err)
	}
	return out, nil
}

func isPlainIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func laterOf(a, b *time.Time) *time.Time {
	if b == nil {
		return a
	}
	if a == nil || b.After(*a) {
		return b
	}
	return a
}

// evaluateKindProgress returns "" when the kind is healthy, else the reason.
// Progress evidence is taken from river_job FIRST (River's own writes) and only
// then from worker_health.last_success_at, so a deployment whose middleware is
// missing reports the truth instead of never_succeeded for everything.
func evaluateKindProgress(row gen.OpenrailsWorkerHealth, kp KindProgress, now time.Time, failureThreshold, staleMultiplier int, minStale time.Duration) string {
	if int(row.ConsecutiveFailures) >= failureThreshold {
		return ProgressConsecutiveFailures
	}
	if kp.ExpectedPeriod <= 0 && (row.ExpectedPeriodSeconds == nil || *row.ExpectedPeriodSeconds <= 0) {
		return "" // on-demand kind: no cadence to be late against
	}
	period := kp.ExpectedPeriod
	if period <= 0 {
		period = time.Duration(*row.ExpectedPeriodSeconds) * time.Second
	}
	threshold := staleThreshold(period, staleMultiplier, minStale)
	progress := laterOf(kp.LastCompletedAt, row.LastSuccessAt)
	if progress == nil {
		if now.Sub(row.RegisteredAt) > threshold {
			return ProgressNeverSucceeded
		}
		return ""
	}
	if now.Sub(*progress) > threshold {
		return ProgressStale
	}
	return ""
}

// workerAlertDue dedupes: alert on first trip, on a fresh incident (progress
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

// RaiseAlerts routes every unhealthy kind — and the fleet-level stall itself —
// to the durable repair-alert channel (notification_queue system alerts, the
// same admin surface the ledger reconcilers use).
func (m *ProgressMonitor) RaiseAlerts(ctx context.Context, report ProgressReport) error {
	if m == nil || m.DB == nil {
		return fmt.Errorf("river progress: DB is required")
	}
	q := m.DB.Gen(ctx)
	rows, err := q.ListWorkerHealth(ctx)
	if err != nil {
		return fmt.Errorf("river progress: list worker health: %w", err)
	}
	byKind := make(map[string]gen.OpenrailsWorkerHealth, len(rows))
	for _, row := range rows {
		byKind[row.WorkerKind] = row
	}

	var alerted, failedAlerts int
	// The fleet-level stall is the #895 signal: it fires even when every
	// per-kind row still looks fine (nothing has been late long enough yet),
	// and it is the ONLY one that can fire while River is completely dead.
	if !report.Progressing {
		fleetRow := byKind[fleetHealthKind]
		fleetRow.WorkerKind = fleetHealthKind
		if workerAlertDue(fleetRow, report.CheckedAt, m.reAlertEvery()) {
			if err := m.raiseAlert(ctx, fleetRow, report.Reason, report.CheckedAt, report); err != nil {
				failedAlerts++
				log.WithContext(ctx).WithError(err).Error("river progress: failed to raise fleet repair alert")
			} else {
				alerted++
				if err := q.SeedWorkerHealth(ctx, gen.SeedWorkerHealthParams{WorkerKind: fleetHealthKind}); err != nil {
					log.WithContext(ctx).WithError(err).Warn("river progress: failed to seed fleet health row")
				}
				if err := q.MarkWorkerHealthAlerted(ctx, gen.MarkWorkerHealthAlertedParams{WorkerKind: fleetHealthKind, Now: report.CheckedAt}); err != nil {
					log.WithContext(ctx).WithError(err).Warn("river progress: failed to mark fleet alerted")
				}
			}
		}
	}

	for _, kp := range report.Kinds {
		if kp.Reason == "" {
			continue
		}
		row, ok := byKind[kp.Kind]
		if !ok {
			row = gen.OpenrailsWorkerHealth{WorkerKind: kp.Kind}
		}
		if !workerAlertDue(row, report.CheckedAt, m.reAlertEvery()) {
			continue
		}
		if err := m.raiseAlert(ctx, row, kp.Reason, report.CheckedAt, report); err != nil {
			failedAlerts++
			log.WithContext(ctx).WithError(err).WithField("worker_kind", kp.Kind).
				Error("river progress: failed to raise repair alert")
			continue
		}
		alerted++
		if err := q.MarkWorkerHealthAlerted(ctx, gen.MarkWorkerHealthAlertedParams{WorkerKind: kp.Kind, Now: report.CheckedAt}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("worker_kind", kp.Kind).
				Warn("river progress: failed to mark alerted")
		}
	}
	if alerted > 0 || failedAlerts > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"alerted": alerted, "failed": failedAlerts, "reason": report.Reason}).
			Warn("river progress: unhealthy periodic fleet")
	}
	if failedAlerts > 0 {
		return fmt.Errorf("river progress: %d repair alerts failed to record", failedAlerts)
	}
	return nil
}

// fleetHealthKind is the pseudo-kind the fleet-level verdict alerts and dedupes
// under. It is NOT a River job kind — nothing enqueues it — it exists so the
// fleet stall gets the same durable, deduped alert row every other kind gets.
const fleetHealthKind = "openrails.river_fleet"

func (m *ProgressMonitor) raiseAlert(ctx context.Context, row gen.OpenrailsWorkerHealth, reason string, now time.Time, report ProgressReport) error {
	// Cross-merchant read on purpose: the alert fans out to every active
	// merchant, so this must be the explicit directory accessor, not a
	// merchant-scoped handle (or#861/or#877 — a bare RLS context reads nothing).
	merchantIDs, err := m.DB.GenDirectory().ListActiveMerchantIDs(ctx)
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
		"detector":             "river_progress_monitor",
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
	if report.FleetLastEnqueuedAt != nil {
		metadata["fleet_last_enqueued_at"] = report.FleetLastEnqueuedAt.UTC().Format(time.RFC3339)
	}
	if report.FleetLastCompletedAt != nil {
		metadata["fleet_last_completed_at"] = report.FleetLastCompletedAt.UTC().Format(time.RFC3339)
	}
	alertErr := fmt.Errorf("worker %s unhealthy: %s", row.WorkerKind, reason)
	if row.WorkerKind == fleetHealthKind {
		alertErr = fmt.Errorf("river periodic fleet is not progressing: %s", reason)
	}
	if row.LastError != nil && *row.LastError != "" {
		alertErr = fmt.Errorf("worker %s unhealthy (%s): %s", row.WorkerKind, reason, *row.LastError)
	}
	alertErrors := make([]error, 0)
	idempotencyKey := workerHealthAlertIdempotencyKey(row, reason)
	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := m.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			return webhooks.RecordLedgerRepairAlert(ctx, m.NotificationService, m.DB, now, webhooks.LedgerRepairAlert{
				Provider:       "openrails",
				Operation:      "river_progress",
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
