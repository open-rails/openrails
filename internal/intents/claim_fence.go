package intents

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrClaimLost: the executor no longer holds the operation. It sends nothing
// and returns to verification.
var ErrClaimLost = errors.New("the executor's claim lapsed or passed to another executor before the provider call")

// claim is the fencing token of one executor run: every claim of a row bumps
// attempts, so a later executor's claim never matches an earlier one.
type claim struct {
	id       uuid.UUID
	status   string
	attempts int32
}

type claimKey struct{}

func withClaim(ctx context.Context, in gen.OpenrailsRailIntent) context.Context {
	return context.WithValue(ctx, claimKey{}, claim{id: in.ID, status: in.Status, attempts: in.Attempts})
}

// RequireClaim re-reads the row on its own connection immediately before a
// provider charge: the run's claim must still be the row's, with a lease past
// now. It narrows, not closes, the window of a stalled executor: the provider
// has no remote fence.
func (s *Store) RequireClaim(ctx context.Context, id uuid.UUID, now time.Time) error {
	c, ok := ctx.Value(claimKey{}).(claim)
	if !ok || c.id != id {
		return ErrClaimLost
	}
	ctx, release, err := s.db.WithIndependentMerchantConn(ctx)
	if err != nil {
		return err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var held bool
	if err := s.db.Qx(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM openrails.rail_intents
		WHERE merchant_id = $1 AND id = $2 AND status = $3 AND attempts = $4 AND claimed_until > $5)`,
		mid.UUID(), id, c.status, c.attempts, now.UTC()).Scan(&held); err != nil {
		return err
	}
	if !held {
		return ErrClaimLost
	}
	return nil
}
