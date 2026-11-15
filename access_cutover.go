package openrails

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
)

// AccessCutoverPreflight reviewed the cutover from per-key entitlement windows
// to product access. The baseline schema already holds product access, so it
// always fails, saying so.
func AccessCutoverPreflight(ctx context.Context, db *pgxpool.Pool, database DatabaseConfig, approvedBy string) (*billing.AccessCutoverReport, error) {
	report, err := engine.AccessCutoverPreflight(ctx, db, Config{Database: database}, approvedBy)
	if err != nil {
		return nil, err
	}
	return &report, nil
}
