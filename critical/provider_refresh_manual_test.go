//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type priorRefreshJob string

func (j priorRefreshJob) Kind() string { return string(j) }

func TestManualProviderRefresh(t *testing.T) {
	t.Run("OR-J38-S72/operator-refresh-replaces-legacy-work-and-retries", func(t *testing.T) {
		a, p, _ := nmiMembership(t)
		ctx := t.Context()
		queue, err := river.NewClient(riverpgxv5.New(a.pool), &river.Config{Schema: a.schema, SkipUnknownJobCheck: true})
		require.NoError(t, err)
		legacyScheduler := priorRefreshJob("openrails.provider_refresh")
		legacyMerchant := priorRefreshJob("openrails.provider_refresh_merchant")
		seed := func(kind priorRefreshJob, opts *river.InsertOpts) int64 {
			// The job kind is old fixture data. The new deployment has no worker for it.
			result, err := queue.Insert(ctx, kind, opts)
			require.NoError(t, err)
			return result.Job.ID
		}
		later := time.Now().Add(24 * time.Hour)
		scheduled := seed(legacyScheduler, &river.InsertOpts{Queue: openrails.QueueBilling, ScheduledAt: later})
		available := seed(legacyMerchant, &river.InsertOpts{Queue: "provider_refresh"})
		history := seed(legacyMerchant, &river.InsertOpts{Queue: openrails.QueueBilling})
		jobsTable := pgx.Identifier{a.schema + "_river", "river_job"}.Sanitize()
		_, err = a.pool.Exec(ctx, "UPDATE "+jobsTable+" SET state = 'completed', finalized_at = now() WHERE id = $1", history)
		require.NoError(t, err)
		unrelated := seed("openrails.cleanup_expired_data", &river.InsertOpts{Queue: openrails.QueueBilling, ScheduledAt: later})

		fleet := hostFleet(t, a.world)
		jobState := func(id int64, state rivertype.JobState) {
			job, err := fleet.JobGet(ctx, id)
			require.NoError(t, err)
			require.Equal(t, state, job.State, "job %d (%s)", id, job.Kind)
		}
		jobState(scheduled, rivertype.JobStateCancelled)
		jobState(available, rivertype.JobStateCancelled)
		jobState(history, rivertype.JobStateCompleted)
		jobState(unrelated, rivertype.JobStateScheduled)

		type receipt struct {
			JobID         int64              `json:"job_id"`
			MerchantID    billing.MerchantID `json:"merchant_id"`
			RiverSchema   string             `json:"river_schema"`
			Queue         string             `json:"queue"`
			AlreadyQueued bool               `json:"already_queued"`
		}
		request := func() receipt {
			var out receipt
			require.NoError(t, json.Unmarshal([]byte(hostCLI(t, a.world, "provider-refresh", "--merchant", "alpha")), &out))
			require.Positive(t, out.JobID)
			require.Equal(t, a.alpha.MerchantID(), out.MerchantID)
			require.Equal(t, a.schema+"_river", out.RiverSchema)
			require.Equal(t, "provider_refresh", out.Queue)
			return out
		}
		var rosterReads atomic.Int32
		var providerDown atomic.Bool
		p.core.FailRequests(func(req *http.Request) bool {
			if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/v5/subscriptions") {
				return false
			}
			rosterReads.Add(1)
			return providerDown.Load()
		}, http.StatusServiceUnavailable, 100)
		providerDown.Store(true)
		require.NoError(t, a.engine.Start(ctx, openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(ctx))
		require.Eventually(t, func() bool {
			jobs, err := fleet.JobList(ctx, river.NewJobListParams().Kinds("openrails.billing_pass").States(rivertype.JobStateCompleted))
			return err == nil && len(jobs.Jobs) > 0
		}, 20*time.Second, 25*time.Millisecond, "startup billing pass finishes")
		jobs, err := fleet.JobList(ctx, river.NewJobListParams().Kinds(legacyScheduler.Kind(), legacyMerchant.Kind(), (refreshMerchantJob{}).Kind()))
		require.NoError(t, err)
		require.Len(t, jobs.Jobs, 3, "starting the workers queues no provider refresh")
		require.Zero(t, rosterReads.Load())

		first := request()
		require.False(t, first.AlreadyQueued)
		duplicate := request()
		require.True(t, duplicate.AlreadyQueued)
		require.Equal(t, first.JobID, duplicate.JobID)
		require.Eventually(t, func() bool {
			job, err := fleet.JobGet(ctx, first.JobID)
			return err == nil && job.State == rivertype.JobStateRetryable && len(job.Errors) > 0 && strings.Contains(job.Errors[0].Error, "incomplete")
		}, 30*time.Second, 25*time.Millisecond, "a provider error leaves the requested job retryable")
		require.Positive(t, rosterReads.Load())
		providerDown.Store(false)
		_, err = fleet.JobRetry(ctx, first.JobID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			job, err := fleet.JobGet(ctx, first.JobID)
			return err == nil && job.State == rivertype.JobStateCompleted
		}, 75*time.Second, 25*time.Millisecond, "the same job resumes and completes")
	})
}
