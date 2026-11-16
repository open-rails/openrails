//go:build integration

package critical_test

import (
	"time"

	"github.com/riverqueue/river/rivertype"
)

// River keeps a job snoozed for no longer than its scheduler's interval
// available with a later scheduled_at, not scheduled. A job waits when it is
// scheduled, retryable or available for later; JobRetry makes any of them
// workable now.
func jobWaits(job *rivertype.JobRow) bool {
	switch job.State {
	case rivertype.JobStateScheduled, rivertype.JobStateRetryable:
		return true
	case rivertype.JobStateAvailable:
		return job.ScheduledAt.After(time.Now())
	}
	return false
}

// jobAsleep reports an available job waiting out a snooze, not queued to run.
// One due within a second is a retry about to run, so it is not asleep.
func jobAsleep(job *rivertype.JobRow) bool {
	return job.State == rivertype.JobStateAvailable && time.Until(job.ScheduledAt) > time.Second
}
