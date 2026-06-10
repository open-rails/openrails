package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
)

// SoftDeleteGraceBySubscription marks grace entitlement windows for a subscription as deleted.
// This is used when a renewal succeeds so the paid subscription window can cover access without
// leaving trailing grace windows.
func (r *EntitlementRepo) SoftDeleteGraceBySubscription(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	return r.db.Gen(ctx).SoftDeleteGraceBySubscription(ctx, gen.SoftDeleteGraceBySubscriptionParams{
		SourceID: subscriptionID,
		Now:      now,
	})
}

// SoftDeleteGraceBySubscriptionTx is the transactional variant.
func (r *EntitlementRepo) SoftDeleteGraceBySubscriptionTx(ctx context.Context, tx pgx.Tx, subscriptionID uuid.UUID, now time.Time) error {
	return gen.New(tx).SoftDeleteGraceBySubscription(ctx, gen.SoftDeleteGraceBySubscriptionParams{
		SourceID: subscriptionID,
		Now:      now,
	})
}

// EndGraceNowBySubscription ends any currently-active grace windows for a subscription immediately (at now),
// and deletes any future grace windows. This is used when dunning is terminal (no more retries).
//
// This operation intentionally affects only grace windows (source_type='grace') and does not revoke
// paid subscription windows.
func (r *EntitlementRepo) EndGraceNowBySubscription(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		rows, err := q.ListActiveGraceWindowsForUpdate(ctx, gen.ListActiveGraceWindowsForUpdateParams{
			SourceID: subscriptionID,
			Now:      now,
		})
		if err != nil {
			return err
		}

		for _, row := range rows {
			// Shorten the grace window to now, shifting later windows in the entitlement timeline
			// so the user's entitlement state remains gapless.
			endAt := now
			if err := SetEntitlementEndAtTx(ctx, tx, row.ID, &endAt, now); err != nil {
				return fmt.Errorf("end grace entitlement: %w", err)
			}
		}

		// Delete any future grace windows for this subscription.
		return q.SoftDeleteFutureGraceBySubscription(ctx, gen.SoftDeleteFutureGraceBySubscriptionParams{
			SourceID: subscriptionID,
			Now:      now,
		})
	})
}
