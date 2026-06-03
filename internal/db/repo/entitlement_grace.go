package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/uptrace/bun"
)

// SoftDeleteGraceBySubscription marks grace entitlement windows for a subscription as deleted.
// This is used when a renewal succeeds so the paid subscription window can cover access without
// leaving trailing grace windows.
func (r *EntitlementRepo) SoftDeleteGraceBySubscription(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	_, err := r.db.Q(ctx).NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("deleted_at = ?", now).
		Set("updated_at = ?", now).
		Where("ent.source_type = ?", models.EntitlementSourceGrace).
		Where("ent.source_id = ?", subscriptionID).
		Where("ent.revoked_at IS NULL").
		Where("ent.deleted_at IS NULL").
		Exec(ctx)
	return err
}

// SoftDeleteGraceBySubscriptionTx is the transactional variant.
func (r *EntitlementRepo) SoftDeleteGraceBySubscriptionTx(ctx context.Context, tx bun.Tx, subscriptionID uuid.UUID, now time.Time) error {
	_, err := tx.NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("deleted_at = ?", now).
		Set("updated_at = ?", now).
		Where("ent.source_type = ?", models.EntitlementSourceGrace).
		Where("ent.source_id = ?", subscriptionID).
		Where("ent.revoked_at IS NULL").
		Where("ent.deleted_at IS NULL").
		Exec(ctx)
	return err
}

// EndGraceNowBySubscription ends any currently-active grace windows for a subscription immediately (at now),
// and deletes any future grace windows. This is used when dunning is terminal (no more retries).
//
// This operation intentionally affects only grace windows (source_type='grace') and does not revoke
// paid subscription windows.
func (r *EntitlementRepo) EndGraceNowBySubscription(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	return r.db.Q(ctx).RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		txdb := db.NewWithTx(tx)
		timelineRepo := NewEntitlementRepo(txdb)

		var rows []models.Entitlement
		if err := tx.NewSelect().
			Model(&rows).
			Where("ent.source_type = ?", models.EntitlementSourceGrace).
			Where("ent.source_id = ?", subscriptionID).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.start_at < ?", now).
			Where("ent.end_at IS NOT NULL AND ent.end_at > ?", now).
			For("UPDATE").
			Scan(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		for _, ent := range rows {
			// Shorten the grace window to now, shifting later windows in the entitlement timeline
			// so the user's entitlement state remains gapless.
			if err := timelineRepo.SetEndAtTx(ctx, tx, ent.ID, &now, now); err != nil {
				return fmt.Errorf("end grace entitlement: %w", err)
			}
		}

		// Delete any future grace windows for this subscription.
		_, err := tx.NewUpdate().
			Model((*models.Entitlement)(nil)).
			Set("deleted_at = ?", now).
			Set("updated_at = ?", now).
			Where("ent.source_type = ?", models.EntitlementSourceGrace).
			Where("ent.source_id = ?", subscriptionID).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.start_at >= ?", now).
			Exec(ctx)
		return err
	})
}
