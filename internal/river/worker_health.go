package riverjobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Per-worker health bookkeeping: WorkerHealthMiddleware rides on every
// OpenRails worker (internal/app.addTrackedWorker) and upserts worker_state
// per job completion. The evaluator that reads these rows (progress.go) is
// not a River job.

// maxWorkerHealthErrorLen bounds last_error (runes, so truncation never splits
// a UTF-8 sequence — Postgres rejects invalid text).
const maxWorkerHealthErrorLen = 2000

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

// WorkerHealthMiddleware is attached to every OpenRails worker: after every
// worked job it upserts the kind's health row. Bookkeeping failures are
// logged, never surfaced as job errors.
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
	// Detached ctx: a canceled/timed-out job must still get its failure recorded.
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

// ListWorkerHealth is every registered worker kind's recent runs, with the
// verbatim error text: the operator's view, never a merchant's.
func ListWorkerHealth(ctx context.Context, database *db.DB) ([]billing.WorkerHealth, error) {
	rows, err := database.GenDirectory().ListWorkerHealth(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]billing.WorkerHealth, 0, len(rows))
	for _, row := range rows {
		out = append(out, billing.WorkerHealth{
			WorkerKind: row.WorkerKind, RegisteredAt: row.RegisteredAt, ExpectedPeriodSeconds: row.ExpectedPeriodSeconds,
			LastSuccessAt: row.LastSuccessAt, LastErrorAt: row.LastErrorAt, LastError: row.LastError,
			ConsecutiveFailures: row.ConsecutiveFailures, LastAlertedAt: row.LastAlertedAt, UpdatedAt: row.UpdatedAt,
		})
	}
	return out, nil
}
