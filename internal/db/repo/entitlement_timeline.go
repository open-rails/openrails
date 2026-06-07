package repo

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/uptrace/bun"
)

func entitlementTimelineLockKey(userID, entitlement string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(entitlement))
	return int64(h.Sum64())
}

func LockEntitlementTimeline(ctx context.Context, tx bun.Tx, userID, entitlement string) error {
	if userID == "" || entitlement == "" {
		return fmt.Errorf("userID and entitlement are required for entitlement timeline lock")
	}
	// Serialize timeline updates per (user_id, entitlement) to prevent overlapping inserts/updates.
	// This is intentionally independent of any particular source_type/source_id.
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", entitlementTimelineLockKey(userID, entitlement))
	return err
}

func ShiftEntitlementTimeline(
	ctx context.Context,
	tx bun.Tx,
	userID string,
	entitlement string,
	from time.Time,
	delta time.Duration,
	now time.Time,
	excludeIDs []uuid.UUID,
) error {
	if delta == 0 {
		return nil
	}
	deltaSeconds := int64(delta.Seconds())
	if deltaSeconds == 0 {
		return nil
	}

	tsid, err := ResolveTenantSubjectID(ctx, tx, uuid.Nil, userID)
	if err != nil {
		return err
	}

	q := tx.NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("start_at = start_at + (? * interval '1 second')", deltaSeconds).
		Set("end_at = CASE WHEN end_at IS NULL THEN NULL ELSE end_at + (? * interval '1 second') END", deltaSeconds).
		Set("updated_at = ?", now).
		Where("ent.tenant_subject_id = ?", tsid).
		Where("ent.entitlement = ?", entitlement).
		Where("ent.revoked_at IS NULL").
		Where("ent.deleted_at IS NULL").
		Where("ent.start_at >= ?", from)

	if len(excludeIDs) > 0 {
		q = q.Where("ent.id NOT IN (?)", bun.In(excludeIDs))
	}

	_, err = q.Exec(ctx)
	return err
}

func getEntitlementByIDTx(ctx context.Context, tx bun.Tx, id uuid.UUID) (*models.Entitlement, error) {
	ent := new(models.Entitlement)
	if err := tx.NewSelect().
		Model(ent).
		Where("ent.id = ?", id).
		Limit(1).
		Scan(ctx); err != nil {
		return nil, err
	}
	return ent, nil
}

func GetEntitlementByIDTx(ctx context.Context, tx bun.Tx, id uuid.UUID) (*models.Entitlement, error) {
	return getEntitlementByIDTx(ctx, tx, id)
}

// SetEntitlementEndAtTx sets end_at on a specific entitlement row, and optionally shifts
// any later windows forward to preserve the no-overlap invariant when extending.
//
// Notes:
//   - Shortening a window does not shift later windows backward (that would be surprising and risky).
//     It may introduce a gap, which is expected for revocations/early termination.
//   - Revoked/deleted rows are ignored for shifting.
func SetEntitlementEndAtTx(ctx context.Context, tx bun.Tx, id uuid.UUID, endAt *time.Time, now time.Time) error {
	ent, err := getEntitlementByIDTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if ent.RevokedAt != nil || ent.DeletedAt != nil {
		return nil
	}

	if endAt != nil {
		e := endAt.UTC()
		endAt = &e
		if !endAt.After(ent.StartAt) {
			return fmt.Errorf("invalid end_at: must be after start_at")
		}
	}

	if err := LockEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement); err != nil {
		return err
	}

	var oldEnd *time.Time
	if ent.EndAt != nil {
		t := ent.EndAt.UTC()
		oldEnd = &t
	}

	_, err = tx.NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("end_at = ?", endAt).
		Set("updated_at = ?", now).
		Where("ent.id = ?", id).
		Where("ent.revoked_at IS NULL").
		Where("ent.deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return err
	}

	// Always keep the timeline gapless:
	// - If end_at moved (extended or shortened), shift later windows by the delta.
	// - If end_at is set to NULL (indefinite), remove later windows (they would overlap).
	if oldEnd != nil && endAt != nil && !endAt.Equal(*oldEnd) {
		delta := endAt.Sub(*oldEnd)
		return ShiftEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement, *oldEnd, delta, now, []uuid.UUID{id})
	}
	if oldEnd != nil && endAt == nil {
		// Indefinite terminates the timeline; delete any later windows.
		tsid, err := ResolveTenantSubjectID(ctx, tx, uuid.Nil, ent.TenantSubjectID.String())
		if err != nil {
			return err
		}
		_, err = tx.NewUpdate().
			Model((*models.Entitlement)(nil)).
			Set("deleted_at = ?", now).
			Set("updated_at = ?", now).
			Where("ent.tenant_subject_id = ?", tsid).
			Where("ent.entitlement = ?", ent.Entitlement).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.start_at >= ?", *oldEnd).
			Where("ent.id <> ?", id).
			Exec(ctx)
		return err
	}
	return nil
}

// RevokeEntitlementNowTx revokes a single entitlement window immediately (at revokeAt) and shifts any later windows
// by the delta so the timeline remains gapless.
func RevokeEntitlementNowTx(
	ctx context.Context,
	tx bun.Tx,
	id uuid.UUID,
	revokeAt time.Time,
	reason *models.EntitlementRevokeReason,
	now time.Time,
) error {
	ent, err := getEntitlementByIDTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if ent.RevokedAt != nil || ent.DeletedAt != nil {
		return nil
	}
	revokeAt = revokeAt.UTC()

	if !revokeAt.After(ent.StartAt) {
		return fmt.Errorf("cannot revoke entitlement: revoke_at must be after start_at")
	}

	if err := LockEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement); err != nil {
		return err
	}

	var oldEnd *time.Time
	if ent.EndAt != nil {
		t := ent.EndAt.UTC()
		oldEnd = &t
	}

	_, err = tx.NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("end_at = ?", revokeAt).
		Set("revoked_at = ?", now).
		Set("revoke_reason = ?", reason).
		Set("updated_at = ?", now).
		Where("ent.id = ?", id).
		Where("ent.revoked_at IS NULL").
		Where("ent.deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return err
	}

	// Keep the timeline gapless by shifting later windows earlier/forward based on the change in end_at.
	if oldEnd != nil && revokeAt.Before(*oldEnd) {
		delta := revokeAt.Sub(*oldEnd) // negative
		return ShiftEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement, *oldEnd, delta, now, []uuid.UUID{id})
	}
	if oldEnd != nil && revokeAt.After(*oldEnd) {
		// Revoking after the scheduled end shouldn't normally happen, but if it does, shift forward.
		delta := revokeAt.Sub(*oldEnd)
		return ShiftEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement, *oldEnd, delta, now, []uuid.UUID{id})
	}
	if oldEnd == nil {
		// Indefinite windows should have no later windows; nothing to shift.
		return nil
	}
	return nil
}
