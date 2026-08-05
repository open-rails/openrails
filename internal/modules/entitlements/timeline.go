package entitlements

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Timeline helpers (#334, relocated from internal/db/repo in #688): the
// pgx/sqlc surface used by EntitlementService for PushNewEntitlement and
// RevokeExistingEntitlement. All take an open transaction (gen.DBTX) so they
// run inside the service's MerchantTx with the timeline advisory lock held.

func entitlementTimelineLockKey(userID, entitlement string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(userID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(entitlement))
	// Mask to 63 bits so the FNV hash is always a non-negative int64. Advisory-lock
	// keys are opaque, so dropping the top bit is harmless and avoids any overflow.
	key, _ := safecast.Convert[int64](h.Sum64() & math.MaxInt64)
	return key
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

	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return err
	}
	if excludeIDs == nil {
		excludeIDs = []uuid.UUID{}
	}
	return gen.New(qx).ShiftEntitlementTimelineWindows(ctx, gen.ShiftEntitlementTimelineWindowsParams{
		CustomerID:   tsid,
		Entitlement:  entitlement,
		DeltaSeconds: deltaSeconds,
		Now:          now,
		FromAt:       from,
		ExcludeIds:   excludeIDs,
	})
}

func GetEntitlementByIDTx(ctx context.Context, qx gen.DBTX, id uuid.UUID) (*models.Entitlement, error) {
	row, err := gen.New(qx).GetEntitlementByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.EntitlementFromGen(row), nil
}

// TimelineHasIndefinite reports whether an unrevoked indefinite window exists
// for (merchant subject, entitlement) — the timeline-terminal state.
func TimelineHasIndefinite(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string) (bool, error) {
	return gen.New(qx).TimelineHasIndefinite(ctx, gen.TimelineHasIndefiniteParams{
		CustomerID: tenantSubjectID, Entitlement: entitlement,
	})
}

// GetTimelineIndefinite returns the (earliest) indefinite window.
func GetTimelineIndefinite(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string) (*models.Entitlement, error) {
	row, err := gen.New(qx).GetTimelineIndefinite(ctx, gen.GetTimelineIndefiniteParams{
		CustomerID: tenantSubjectID, Entitlement: entitlement,
	})
	if err != nil {
		return nil, err
	}
	return models.EntitlementFromGen(row), nil
}

// GetTimelineTailEnd returns the latest finite end_at on the timeline, or nil
// when the timeline has no finite windows.
func GetTimelineTailEnd(ctx context.Context, qx gen.DBTX, tenantSubjectID uuid.UUID, entitlement string) (*time.Time, error) {
	end, err := gen.New(qx).GetTimelineTailEnd(ctx, gen.GetTimelineTailEndParams{
		CustomerID: tenantSubjectID, Entitlement: entitlement,
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
		CustomerID: tenantSubjectID, Entitlement: entitlement, At: at,
	})
	if err != nil {
		return nil, err
	}
	return models.EntitlementFromGen(row), nil
}

// GetEntitlementByGrant returns the entitlement window MaterializeGrant projected
// for a given grant + feature (#511 fetch-back), or ErrNoRows.
func GetEntitlementByGrant(ctx context.Context, qx gen.DBTX, merchantID, grantID uuid.UUID, entitlement string) (*models.Entitlement, error) {
	row, err := gen.New(qx).GetEntitlementByGrant(ctx, gen.GetEntitlementByGrantParams{
		MerchantID: merchantID, GrantID: grantID, Entitlement: entitlement,
	})
	if err != nil {
		return nil, err
	}
	return models.EntitlementFromGen(row), nil
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
		CustomerID: tenantSubjectID, Entitlement: entitlement,
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
		CustomerID: tenantSubjectID, Entitlement: entitlement,
		Now: now, SourceType: st, SourceID: sourceID,
	})
}
