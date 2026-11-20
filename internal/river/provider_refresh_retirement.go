package riverjobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// RetireLegacyProviderRefresh cancels the old scheduler and merchant jobs in
// the client's schema. Stop old workers before upgrading so they cannot start
// another refresh or add jobs while the new fleet binds. A running row gets
// River's cancellation signal and cannot retry, including after a rescue.
func RetireLegacyProviderRefresh(ctx context.Context, client *river.Client[pgx.Tx]) (int, error) {
	const pageSize = 100
	params := river.NewJobListParams().Kinds("openrails.provider_refresh", "openrails.provider_refresh_merchant").
		States(rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRetryable,
			rivertype.JobStateRunning, rivertype.JobStateScheduled).
		OrderBy(river.JobListOrderByID, river.SortOrderDesc).First(pageSize)

	// Descending IDs bound the pass even if an old producer inserts more jobs.
	// The cursor also advances past running rows awaiting worker cancellation.
	var requested int
	for {
		page, err := client.JobList(ctx, params)
		if err != nil {
			return requested, fmt.Errorf("list legacy provider refresh jobs: %w", err)
		}
		for _, job := range page.Jobs {
			current, err := client.JobCancel(ctx, job.ID)
			if errors.Is(err, rivertype.ErrNotFound) {
				continue
			}
			if err != nil {
				return requested, fmt.Errorf("cancel legacy provider refresh job %d: %w", job.ID, err)
			}
			if current.State == rivertype.JobStateCancelled || current.State == rivertype.JobStateRunning {
				requested++
			}
		}
		if len(page.Jobs) < pageSize {
			return requested, nil
		}
		params = params.After(page.LastCursor)
	}
}
