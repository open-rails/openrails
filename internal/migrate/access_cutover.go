package migrate

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
)

// ErrNoAccessCutover: the baseline schema already holds product access, so no
// migration converts per-key entitlement windows and nothing is left to review.
var ErrNoAccessCutover = errors.New("openrails: there is no product access cutover to preflight: the baseline schema already holds product access, and a database from before it is recreated, not upgraded")

// AccessCutoverPreflight always fails with ErrNoAccessCutover.
func AccessCutoverPreflight(context.Context, *pgxpool.Pool, string, string) (billing.AccessCutoverReport, error) {
	return billing.AccessCutoverReport{Changes: []billing.AccessChange{}, Notes: []billing.AccessNote{}}, ErrNoAccessCutover
}
