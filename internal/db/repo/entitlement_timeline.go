package repo

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// --- Timeline-service helpers (#334): the pgx/sqlc surface used by
// modules/entitlements.EntitlementService for PushNewEntitlement and
// RevokeExistingEntitlement. All take an open transaction (gen.DBTX) so they
// run inside the service's TenantTx with the timeline advisory lock held.

// TimelineHasIndefinite reports whether an unrevoked indefinite window exists
// for (tenant subject, entitlement) — the timeline-terminal state.
func TimelineHasIndefinite(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string) (bool, error) {
	return gen.New(qx).TimelineHasIndefinite(ctx, gen.TimelineHasIndefiniteParams{
		TenantSubjectID: tenantSubjectID, Entitlement: entitlement,
	})
}

// GetTimelineIndefinite returns the (earliest) indefinite window.
func GetTimelineIndefinite(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string) (*models.Entitlement, error) {
	row, err := gen.New(qx).GetTimelineIndefinite(ctx, gen.GetTimelineIndefiniteParams{
		TenantSubjectID: tenantSubjectID, Entitlement: entitlement,
	})
	if err != nil {
		return nil, err
	}
	return entitlementFromGen(row), nil
}

// GetTimelineTailEnd returns the latest finite end_at on the timeline, or nil
// when the timeline has no finite windows.
func GetTimelineTailEnd(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string) (*time.Time, error) {
	end, err := gen.New(qx).GetTimelineTailEnd(ctx, gen.GetTimelineTailEndParams{
		TenantSubjectID: tenantSubjectID, Entitlement: entitlement,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return end, nil
}

// GetTimelineCoveringWindow returns the window covering instant `at`
// (pgx.ErrNoRows when none does).
func GetTimelineCoveringWindow(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (*models.Entitlement, error) {
	row, err := gen.New(qx).GetTimelineCoveringWindow(ctx, gen.GetTimelineCoveringWindowParams{
		TenantSubjectID: tenantSubjectID, Entitlement: entitlement, At: at,
	})
	if err != nil {
		return nil, err
	}
	return entitlementFromGen(row), nil
}

// InsertTimelineWindow persists a fully-populated new window.
func InsertTimelineWindow(ctx context.Context, qx gen.DBTX, ent *models.Entitlement) error {
	var sourceID *uuid.UUID
	if ent.SourceID != nil && *ent.SourceID != uuid.Nil {
		sourceID = ent.SourceID
	}
	_, err := gen.New(qx).CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              ent.ID,
		TenantID:        ent.TenantID,
		TenantSubjectID: ent.TenantSubjectID,
		Entitlement:     ent.Entitlement,
		StartAt:         ent.StartAt,
		EndAt:           ent.EndAt,
		SourceID:        sourceID,
		SourceType:      string(ent.SourceType),
		RevokedAt:       ent.RevokedAt,
		RevokeReason:    revokeReasonPtr(ent.RevokeReason),
		CreatedAt:       ent.CreatedAt,
		UpdatedAt:       ent.UpdatedAt,
	})
	return err
}

// AttachTenantSubjectIfMissing backfills tenant_subject_id onto a legacy row
// (#317); no-op when the row already carries one.
func AttachTenantSubjectIfMissing(ctx context.Context, qx gen.DBTX, ent *models.Entitlement, tenantSubjectID uuid.UUID, now time.Time) error {
	if ent == nil || tenantSubjectID == uuid.Nil || ent.TenantSubjectID != uuid.Nil {
		return nil
	}
	if err := gen.New(qx).AttachEntitlementTenantSubject(ctx, gen.AttachEntitlementTenantSubjectParams{
		ID: ent.ID, TenantSubjectID: tenantSubjectID, UpdatedAt: now,
	}); err != nil {
		return err
	}
	ent.TenantSubjectID = tenantSubjectID
	ent.UpdatedAt = now
	return nil
}

// SoftDeleteEntitlementByID soft-deletes one (future) window.
func SoftDeleteEntitlementByID(ctx context.Context, qx gen.DBTX, id uuid.UUID, now time.Time) error {
	return gen.New(qx).SoftDeleteEntitlementByID(ctx, gen.SoftDeleteEntitlementByIDParams{ID: id, Now: now})
}

// RevokeEntitlementByID revokes one active window with a reason.
func RevokeEntitlementByID(ctx context.Context, qx gen.DBTX, id uuid.UUID, reason models.EntitlementRevokeReason, now time.Time) error {
	_, err := gen.New(qx).RevokeEntitlementByID(ctx, gen.RevokeEntitlementByIDParams{
		ID: id, Now: now, RevokeReason: string(reason),
	})
	return err
}

// RevokeActiveTimelineWindows revokes every currently-active window on the
// timeline; nil source filters mean "any source".
func RevokeActiveTimelineWindows(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string, reason models.EntitlementRevokeReason, sourceType *models.EntitlementSourceType, sourceID *uuid.UUID, now time.Time) error {
	var st *string
	if sourceType != nil {
		v := string(*sourceType)
		st = &v
	}
	if sourceID != nil && *sourceID == uuid.Nil {
		sourceID = nil
	}
	return gen.New(qx).RevokeActiveTimelineWindows(ctx, gen.RevokeActiveTimelineWindowsParams{
		TenantSubjectID: tenantSubjectID, Entitlement: entitlement,
		Now: now, RevokeReason: string(reason), SourceType: st, SourceID: sourceID,
	})
}

// SoftDeleteFutureTimelineWindows soft-deletes every future scheduled window
// on the timeline; nil source filters mean "any source".
func SoftDeleteFutureTimelineWindows(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string, sourceType *models.EntitlementSourceType, sourceID *uuid.UUID, now time.Time) error {
	var st *string
	if sourceType != nil {
		v := string(*sourceType)
		st = &v
	}
	if sourceID != nil && *sourceID == uuid.Nil {
		sourceID = nil
	}
	return gen.New(qx).SoftDeleteFutureTimelineWindows(ctx, gen.SoftDeleteFutureTimelineWindowsParams{
		TenantSubjectID: tenantSubjectID, Entitlement: entitlement,
		Now: now, SourceType: st, SourceID: sourceID,
	})
}
