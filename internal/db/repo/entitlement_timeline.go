package repo

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

func entitlementTimelineLockKey(userID, entitlement string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(entitlement))
	return int64(h.Sum64())
}

// LockEntitlementTimeline serializes timeline updates per (user_id,
// entitlement) to prevent overlapping inserts/updates. This is intentionally
// independent of any particular source_type/source_id. qx MUST be an open
// transaction (pg_advisory_xact_lock is transaction-scoped).
func LockEntitlementTimeline(ctx context.Context, qx gen.DBTX, userID, entitlement string) error {
	if userID == "" || entitlement == "" {
		return fmt.Errorf("userID and entitlement are required for entitlement timeline lock")
	}
	return gen.New(qx).AcquireEntitlementTimelineLock(ctx, entitlementTimelineLockKey(userID, entitlement))
}

func ShiftEntitlementTimeline(
	ctx context.Context,
	qx gen.DBTX,
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

	tsid, err := ResolveTenantSubjectID(ctx, qx, uuid.Nil, userID)
	if err != nil {
		return err
	}
	if excludeIDs == nil {
		excludeIDs = []uuid.UUID{}
	}
	return gen.New(qx).ShiftEntitlementTimelineWindows(ctx, gen.ShiftEntitlementTimelineWindowsParams{
		TenantSubjectID: tsid,
		Entitlement:     entitlement,
		DeltaSeconds:    deltaSeconds,
		Now:             now,
		FromAt:          from,
		ExcludeIds:      excludeIDs,
	})
}

func getEntitlementByIDTx(ctx context.Context, qx gen.DBTX, id uuid.UUID) (*models.Entitlement, error) {
	row, err := gen.New(qx).GetEntitlementByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return entitlementFromGen(row), nil
}

func GetEntitlementByIDTx(ctx context.Context, qx gen.DBTX, id uuid.UUID) (*models.Entitlement, error) {
	return getEntitlementByIDTx(ctx, qx, id)
}

// SetEntitlementEndAtTx sets end_at on a specific entitlement row, and optionally shifts
// any later windows forward to preserve the no-overlap invariant when extending.
//
// Notes:
//   - Shortening a window does not shift later windows backward (that would be surprising and risky).
//     It may introduce a gap, which is expected for revocations/early termination.
//   - Revoked/deleted rows are ignored for shifting.
func SetEntitlementEndAtTx(ctx context.Context, qx gen.DBTX, id uuid.UUID, endAt *time.Time, now time.Time) error {
	ent, err := getEntitlementByIDTx(ctx, qx, id)
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

	if err := LockEntitlementTimeline(ctx, qx, ent.TenantSubjectID.String(), ent.Entitlement); err != nil {
		return err
	}

	var oldEnd *time.Time
	if ent.EndAt != nil {
		t := ent.EndAt.UTC()
		oldEnd = &t
	}

	q := gen.New(qx)
	if err := q.SetEntitlementEndAt(ctx, gen.SetEntitlementEndAtParams{
		ID:    id,
		EndAt: endAt,
		Now:   now,
	}); err != nil {
		return err
	}

	// Always keep the timeline gapless:
	// - If end_at moved (extended or shortened), shift later windows by the delta.
	// - If end_at is set to NULL (indefinite), remove later windows (they would overlap).
	if oldEnd != nil && endAt != nil && !endAt.Equal(*oldEnd) {
		delta := endAt.Sub(*oldEnd)
		return ShiftEntitlementTimeline(ctx, qx, ent.TenantSubjectID.String(), ent.Entitlement, *oldEnd, delta, now, []uuid.UUID{id})
	}
	if oldEnd != nil && endAt == nil {
		// Indefinite terminates the timeline; delete any later windows.
		tsid, err := ResolveTenantSubjectID(ctx, qx, uuid.Nil, ent.TenantSubjectID.String())
		if err != nil {
			return err
		}
		return q.SoftDeleteLaterEntitlementWindows(ctx, gen.SoftDeleteLaterEntitlementWindowsParams{
			TenantSubjectID: tsid,
			Entitlement:     ent.Entitlement,
			Now:             now,
			FromAt:          *oldEnd,
			ExcludeID:       id,
		})
	}
	return nil
}

// RevokeEntitlementNowTx revokes a single entitlement window immediately (at revokeAt) and shifts any later windows
// by the delta so the timeline remains gapless.
func RevokeEntitlementNowTx(
	ctx context.Context,
	qx gen.DBTX,
	id uuid.UUID,
	revokeAt time.Time,
	reason *models.EntitlementRevokeReason,
	now time.Time,
) error {
	ent, err := getEntitlementByIDTx(ctx, qx, id)
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

	if err := LockEntitlementTimeline(ctx, qx, ent.TenantSubjectID.String(), ent.Entitlement); err != nil {
		return err
	}

	var oldEnd *time.Time
	if ent.EndAt != nil {
		t := ent.EndAt.UTC()
		oldEnd = &t
	}

	if err := gen.New(qx).RevokeEntitlementWindowNow(ctx, gen.RevokeEntitlementWindowNowParams{
		ID:           id,
		EndAt:        revokeAt,
		Now:          now,
		RevokeReason: revokeReasonPtr(reason),
	}); err != nil {
		return err
	}

	// Keep the timeline gapless by shifting later windows earlier/forward based on the change in end_at.
	if oldEnd != nil && !revokeAt.Equal(*oldEnd) {
		delta := revokeAt.Sub(*oldEnd)
		return ShiftEntitlementTimeline(ctx, qx, ent.TenantSubjectID.String(), ent.Entitlement, *oldEnd, delta, now, []uuid.UUID{id})
	}
	// Indefinite windows should have no later windows; nothing to shift.
	return nil
}
