package openrails

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
)

// AccessCutoverPreflight is the review step of the cutover from per-key
// entitlement windows to product access. New runs the cutover as it migrates
// and fails to boot while it would change a customer's access no one
// approved, so a host binary offers this step and runs it before starting the
// new version. It applies the migrations before the cutover through db,
// dry-runs the cutover for every merchant in database.Schema and reports each
// customer's keys lost or gained, the windows it cannot carry (unmapped) and
// purchases with mixed durations. An empty approvedBy is a dry run; otherwise
// every listed change is recorded as approved by that name, and the cutover
// applies exactly that list. It fails once the cutover has run.
func AccessCutoverPreflight(ctx context.Context, db *pgxpool.Pool, database DatabaseConfig, approvedBy string) (*billing.AccessCutoverReport, error) {
	report, err := engine.AccessCutoverPreflight(ctx, db, Config{Database: database}, approvedBy)
	if err != nil {
		return nil, err
	}
	return &report, nil
}
