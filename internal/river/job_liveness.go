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

	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// No River job runs under a clock: a job is stopped by observed lack of
// progress, never by elapsed time. The middleware rides on every OpenRails
// worker, so a host's river.Config cannot omit it:
//
//   - JobTimeout is -1 (River's "never") on the wrapper worker and on the
//     standalone client.
//   - Liveness: while a job runs, the middleware beats river_job.attempted_at.
//     JobRescueWorker returns a running job only once its beat stops, so a
//     live long job is never re-enqueued and run twice.
//   - Progress: workers report units of work through progress.Mark. A job
//     silent past its kind's staleness threshold (staleThreshold, shared with
//     the fleet monitor) is wedged and canceled with a NoProgressError naming
//     the last thing it reported.
//
// A job that dies with its process stops beating and is rescued; one alive
// but wedged keeps beating and is canceled here; one progressing runs as long
// as the work takes.

// JobLivenessBeat is the beat cadence. It must be short against
// JobRescueSilence so a live job is never taken for dead; it also sets how
// promptly a wedged job is noticed.
const JobLivenessBeat = time.Minute

// progress is a leaf package, so the intent runner and the reconcile engine
// can mark progress without importing this one.

// NoProgressError is the reason a wedged job was canceled: what it last
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
	// River grants the beat access to river_job. Nil disables the beat (logged
	// once): a long job is then rescued while alive and may run twice.
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
			"silence":     cadence.FormatDuration(silence.Round(time.Second)),
			"threshold":   cadence.FormatDuration(threshold),
			"marks":       reason.Marks,
			"last_note":   reason.LastNote,
		}).Error("river: cancelling job — no observed progress")
		cancel(reason)
		return
	}
}

// beatLiveness refreshes river_job.attempted_at, which JobRescueWorker reads,
// for the running job. Raw SQL against River's own table (see
// internal/db/queries/EXEMPTIONS.md): River has no heartbeat API.
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
	// the instant the job is being canceled, and a beat is a single indexed
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
