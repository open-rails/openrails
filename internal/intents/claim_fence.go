package intents

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrClaimLost: the executor no longer holds the operation. It sends nothing
// and returns to verification.
var ErrClaimLost = errors.New("the executor's claim lapsed or passed to another executor before the provider call")

// claim is the fencing token of one executor run: every claim of a row bumps
// attempts, so a later executor's claim never matches an earlier one. lost is
// set when the run's heartbeat finds the claim gone.
type claim struct {
	id       uuid.UUID
	status   string
	attempts int32
	lost     atomic.Bool
}

func (c *claim) lose() { c.lost.Store(true) }

type claimKey struct{}

func withClaim(ctx context.Context, in gen.OpenrailsRailIntent) (context.Context, *claim) {
	c := &claim{id: in.ID, status: in.Status, attempts: in.Attempts}
	return context.WithValue(ctx, claimKey{}, c), c
}

// ProviderCallHold is the lease a charge still needs when it starts: the
// longest provider call (NMI's 25s mutation timeout) plus slack. No other
// executor can claim the row until the call has returned or timed out.
const ProviderCallHold = 30 * time.Second

// RequireClaim re-reads the row on its own connection immediately before a
// provider charge: the run must hold an executor claim (verification never
// charges), its heartbeat must not have lost it, and the row must still carry
// it with a lease past now + ProviderCallHold, so the lease cannot lapse
// while the call is in flight. The provider has no remote fence; this is the
// bound on a stalled executor.
func (s *Store) RequireClaim(ctx context.Context, id uuid.UUID, now time.Time) error {
	c, ok := ctx.Value(claimKey{}).(*claim)
	if !ok || c.id != id || c.status != StatusInFlight || c.lost.Load() {
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
		mid.UUID(), id, c.status, c.attempts, now.UTC().Add(ProviderCallHold)).Scan(&held); err != nil {
		return err
	}
	if !held {
		return ErrClaimLost
	}
	return nil
}
