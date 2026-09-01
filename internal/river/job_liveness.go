package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/shared/progress"
)

// xs-007 row 31: no River job runs under a clock.
//
// River's client default is JobTimeoutDefault = 1 minute, and nothing in
// OpenRails ever overrode it, so EVERY job — dunning across every merchant,
// cleanup, provider refresh, account-updater batches, ledger integrity — had
// its context cancelled at 60 s. The worst case is a charge that landed at
// NMI at t=60 whose bookkeeping never ran. The number appeared nowhere in
// OpenRails code, docs or tests.
//
// The rule (owner ruling, xs-007): a job is stopped by OBSERVED lack of
// progress, never by elapsed time. This middleware is how that rule is
// enforced for every OpenRails worker, with no host cooperation (#895 posture:
// it rides on the worker, so a host's own river.Config cannot omit it):
//
//   - JobTimeout is -1 (River's "never") on the wrapper worker AND on the
//     standalone client. Duration is not evidence.
//   - LIVENESS: while a job runs, the middleware beats River's own
//     river_job.attempted_at. That column is the ONE thing River's JobRescuer
//     reads to call a job stuck (`state='running' AND attempted_at < now -
//     RescueStuckJobsAfter`), so the rescue horizon becomes "silence from a
//     dead process", measured from the last beat, instead of a cap on how long
//     a live job may run. Without the beat, a live 61-minute job is re-enqueued
//     and runs TWICE — the duplicate-execution class River's own docs warn of.
//   - PROGRESS: workers report units of work through progress.Mark. A job that
//     stays silent past the same staleness rule the fleet monitor applies to
//     its kind (k x declared cadence, floored — progress.go, one function) is
//     wedged, and its context is cancelled with a NoProgressError that names
//     the last thing it reported. "A clock reading is not a death certificate"
//     (jobs_dunning.go); silence is.
//
// A job that dies WITH its process stops beating and stops marking; River's
// rescuer takes it back at its horizon. A job that is alive but wedged keeps
// beating (so it is never duplicated) and is cancelled here (so it does not
// hold a worker slot forever). A job that is alive and progressing runs for as
// long as the work takes.

// JobLivenessBeat is the beat cadence: a poll interval, not a decision. It has
// to be short against River's rescue horizon (1 h by default, never below a
// host's JobTimeout) so a live job is never mistaken for a dead one; it also
// sets how promptly a wedged job is noticed. A minute serves both.
const JobLivenessBeat = time.Minute

// Workers report progress with progress.Mark(ctx, note) (internal/shared/
// progress — a leaf package, so the intent runner and the reconcile engine can
// mark on the context they were handed without importing this one).

// NoProgressError is the reason a wedged job was cancelled: what it last
// reported, and how long ago. It is what the job row's error records.
type NoProgressError struct {
	Kind      string
	JobID     int64
	Silence   time.Duration
	Threshold time.Duration
	Marks     int64
	LastNote  string
}

func (e *NoProgressError) Error() string {
	last := "no progress reported since start"
	if e.Marks > 0 {
		last = fmt.Sprintf("last progress %q (%d marks)", e.LastNote, e.Marks)
	}
	return fmt.Sprintf("river: %s job %d reaped for no observed progress in %s (threshold %s; %s)",
		e.Kind, e.JobID, e.Silence.Round(time.Second), e.Threshold, last)
}

// RiverTableAccess resolves how the beat reaches River's own river_job table:
// the pool River itself writes through and the schema its tables live in. It
// is a function because on an embedded host both are only known once the
// host's client is bound, after the workers were registered.
type RiverTableAccess func() (pool *pgxpool.Pool, schema string)

// JobLivenessMiddleware is attached to every OpenRails worker at registration
// (internal/app.addTrackedWorker). See the file comment.
type JobLivenessMiddleware struct {
	river.MiddlewareDefaults
	// River grants the beat access to river_job. Nil disables the beat and is
	// logged once, loudly: River's rescuer then measures from attempt start,
	// which is the duplicate-execution exposure this file exists to remove.
	River RiverTableAccess
	// Registrations supplies each kind's declared cadence, the base of the
	// staleness rule. Nil means every kind is on-demand (floor only).
	Registrations *WorkerRegistrations

	// Tunables; zero values take the same defaults as ProgressMonitor so there
	// is exactly one staleness rule in this package.
	Beat            time.Duration
	StaleMultiplier int
	MinStale        time.Duration

	warnNoAccess sync.Once
}

// NewJobLivenessMiddleware builds the middleware for one worker.
func NewJobLivenessMiddleware(access RiverTableAccess, regs *WorkerRegistrations) *JobLivenessMiddleware {
	return &JobLivenessMiddleware{River: access, Registrations: regs}
}

func (m *JobLivenessMiddleware) beat() time.Duration {
	if m.Beat > 0 {
		return m.Beat
	}
	return JobLivenessBeat
}

func (m *JobLivenessMiddleware) staleMultiplier() int {
	if m.StaleMultiplier > 0 {
		return m.StaleMultiplier
	}
	return defaultStaleMultiplier
}

func (m *JobLivenessMiddleware) minStale() time.Duration {
	if m.MinStale > 0 {
		return m.MinStale
	}
	return defaultMinStale
}

// threshold is the kind's silence tolerance: the fleet monitor's staleness
// rule applied to one running job.
func (m *JobLivenessMiddleware) threshold(kind string) time.Duration {
	var period time.Duration
	if m.Registrations != nil {
		period = m.Registrations.Snapshot()[kind]
	}
	return staleThreshold(period, m.staleMultiplier(), m.minStale())
}

func (m *JobLivenessMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	if m == nil || job == nil {
		return doInner(ctx)
	}
	tracker := progress.NewTracker(time.Now())
	ctx = progress.WithTracker(ctx, tracker)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.watch(ctx, job, tracker, cancel)
	}()

	err := doInner(ctx)
	cancel(nil)
	<-done

	var reaped *NoProgressError
	if cause := context.Cause(ctx); errors.As(cause, &reaped) {
		switch {
		case err == nil:
			// The worker finished its work at the boundary; done is done —
			// failing it would retry completed work.
			return nil
		case errors.Is(err, cause):
			return err
		default:
			return fmt.Errorf("%w (worker returned: %v)", reaped, err)
		}
	}
	return err
}

// watch beats liveness and judges progress until the job ends.
func (m *JobLivenessMiddleware) watch(ctx context.Context, job *rivertype.JobRow, tracker *progress.Tracker, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(m.beat())
	defer ticker.Stop()
	threshold := m.threshold(job.Kind)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.beatLiveness(ctx, job)
		silence := time.Since(tracker.LastMark())
		if silence <= threshold {
			continue
		}
		reason := &NoProgressError{
			Kind: job.Kind, JobID: job.ID, Silence: silence, Threshold: threshold,
			Marks: tracker.Marks(), LastNote: tracker.LastNote(),
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"event":       "river_job_reaped_no_progress",
			"worker_kind": job.Kind,
			"job_id":      job.ID,
			"attempt":     job.Attempt,
			"silence":     silence.Round(time.Second).String(),
			"threshold":   threshold.String(),
			"marks":       reason.Marks,
			"last_note":   reason.LastNote,
		}).Error("river: cancelling job — no observed progress")
		cancel(reason)
		return
	}
}

// beatLiveness refreshes river_job.attempted_at for the running job. Raw SQL
// against River's OWN table (see internal/db/queries/EXEMPTIONS.md): the
// column is River's, the schema is River's, and the update is the only way to
// tell River's rescuer "still here" — it has no heartbeat API.
func (m *JobLivenessMiddleware) beatLiveness(ctx context.Context, job *rivertype.JobRow) {
	if m.River == nil {
		m.warnNoAccess.Do(func() {
			log.WithField("worker_kind", job.Kind).Warn(
				"river liveness: no river_job access wired; River's rescuer will measure this job from attempt start (xs-007 row 31)")
		})
		return
	}
	pool, schema := m.River()
	if pool == nil {
		m.warnNoAccess.Do(func() {
			log.WithField("worker_kind", job.Kind).Warn(
				"river liveness: no pool for river_job; River's rescuer will measure this job from attempt start (xs-007 row 31)")
		})
		return
	}
	schema = strings.TrimSpace(schema)
	if schema == "" {
		schema = "public"
	}
	if !isPlainIdentifier(schema) {
		log.WithField("schema", schema).Error("river liveness: refusing unsafe river schema")
		return
	}
	// Detached from the job's context on purpose: the beat must land even in
	// the instant the job is being cancelled, and a beat is a single indexed
	// UPDATE by primary key.
	ctx, stop := context.WithTimeout(context.WithoutCancel(ctx), m.beat())
	defer stop()
	sql := fmt.Sprintf(`UPDATE %s.river_job SET attempted_at = now() WHERE id = $1 AND state = 'running'`, schema)
	if _, err := pool.Exec(ctx, sql, job.ID); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"worker_kind": job.Kind, "job_id": job.ID,
		}).Warn("river liveness: beat failed")
	}
}
