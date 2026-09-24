package riverjobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

// KindJobRescue returns OpenRails jobs orphaned by a dead process to work.
const KindJobRescue = "openrails.job_rescue"

// JobRescueSilence is how long a running OpenRails job may go without a
// liveness beat before its process is taken as dead. Live jobs beat every
// JobLivenessBeat, so a job silent for several beats has no process.
const JobRescueSilence = 5 * JobLivenessBeat

// JobRescueArgs runs one rescue pass.
type JobRescueArgs struct{}

func (JobRescueArgs) Kind() string { return KindJobRescue }

// JobRescueWorker is the rescue River's own rescuer no longer performs for
// OpenRails jobs: they run with no elapsed-time timeout (xs-007) and River
// v0.47 never rescues a job whose timeout is negative, so a job killed with
// its process stayed running forever and its operation never resumed. The
// liveness beat is the evidence: a job whose beat stopped is dead. Rescued
// work re-enters through its own recovery (an operation re-reads the
// provider before any write), never as a blind re-send.
type JobRescueWorker struct {
	river.WorkerDefaults[JobRescueArgs]
	River   RiverTableAccess
	Silence time.Duration
}

func (JobRescueWorker) Kind() string { return KindJobRescue }

func (w JobRescueWorker) Work(ctx context.Context, _ *river.Job[JobRescueArgs]) error {
	silence := w.Silence
	if silence <= 0 {
		silence = JobRescueSilence
	}
	n, err := RescueSilentJobs(ctx, w.River, silence)
	if err != nil {
		return err
	}
	if n > 0 {
		log.WithContext(ctx).WithField("rescued", n).Warn("river rescue: returned OpenRails jobs whose process stopped beating")
	}
	return nil
}

// RescueSilentJobs makes every running OpenRails job whose beat stopped
// longer than silence ago available again, recording the rescue as River's
// own rescuer does. Raw SQL against River's OWN table, like the beat.
func RescueSilentJobs(ctx context.Context, access RiverTableAccess, silence time.Duration) (int64, error) {
	if access == nil {
		return 0, fmt.Errorf("river rescue: no river_job access wired")
	}
	pool, schema := access()
	if pool == nil {
		return 0, fmt.Errorf("river rescue: no pool for river_job")
	}
	schema = strings.TrimSpace(schema)
	if schema == "" {
		schema = "public"
	}
	if !isPlainIdentifier(schema) {
		return 0, fmt.Errorf("river rescue: refusing unsafe river schema %q", schema)
	}
	sql := fmt.Sprintf(`UPDATE %s.river_job
		SET state = 'available',
		    scheduled_at = now(),
		    errors = array_append(errors, jsonb_build_object('at', now(), 'attempt', attempt, 'error', 'Silent job rescued by OpenRails: its process stopped beating', 'trace', '')),
		    metadata = metadata || jsonb_build_object('openrails:rescue_count', coalesce((metadata ->> 'openrails:rescue_count')::int, 0) + 1)
		WHERE state = 'running'
		  AND kind LIKE 'openrails.%%'
		  AND attempted_at < now() - make_interval(secs => $1)`, schema)
	tag, err := pool.Exec(ctx, sql, silence.Seconds())
	if err != nil {
		return 0, fmt.Errorf("river rescue: %w", err)
	}
	return tag.RowsAffected(), nil
}
