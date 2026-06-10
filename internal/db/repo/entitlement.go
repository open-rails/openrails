package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

type EntitlementRepo struct {
	db *db.DB
}

func NewEntitlementRepo(d *db.DB) *EntitlementRepo { return &EntitlementRepo{db: d} }

func (r *EntitlementRepo) SetEndAtTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, endAt *time.Time, now time.Time) error {
	return SetEntitlementEndAtTx(ctx, tx, id, endAt, now)
}

func (r *EntitlementRepo) IsEntitled(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return false, err
	}
	return r.IsTenantSubjectEntitled(ctx, tsid, entitlement, at)
}

func (r *EntitlementRepo) IsTenantSubjectEntitled(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (bool, error) {
	// Tenant scoping (issue #223): defaults to the default tenant when the
	// request has not resolved one, so single-tenant runs unchanged.
	return r.db.Gen(ctx).EntitlementExistsActive(ctx, gen.EntitlementExistsActiveParams{
		TenantID:        tenant.FromContextOrDefault(ctx).UUID(),
		TenantSubjectID: tenantSubjectID,
		Entitlement:     entitlement,
		At:              at,
	})
}

func (r *EntitlementRepo) HasActiveIndefinite(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return false, err
	}
	return r.HasActiveIndefiniteByTenantSubject(ctx, tsid, entitlement, at)
}

func (r *EntitlementRepo) HasActiveIndefiniteByTenantSubject(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (bool, error) {
	return r.db.Gen(ctx).EntitlementHasActiveIndefinite(ctx, gen.EntitlementHasActiveIndefiniteParams{
		TenantID:        tenant.FromContextOrDefault(ctx).UUID(),
		TenantSubjectID: tenantSubjectID,
		Entitlement:     entitlement,
		At:              at,
	})
}

func (r *EntitlementRepo) GetLatestActive(ctx context.Context, userID, entitlement string) (*models.Entitlement, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	return r.GetLatestActiveByTenantSubject(ctx, tsid, entitlement)
}

func (r *EntitlementRepo) GetLatestActiveByTenantSubject(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string) (*models.Entitlement, error) {
	row, err := r.db.Gen(ctx).GetLatestActiveEntitlement(ctx, gen.GetLatestActiveEntitlementParams{
		TenantID:        tenant.FromContextOrDefault(ctx).UUID(),
		TenantSubjectID: tenantSubjectID,
		Entitlement:     entitlement,
	})
	if err != nil {
		return nil, err
	}
	return entitlementFromGen(row), nil
}

func (r *EntitlementRepo) GetLatestFiniteActive(ctx context.Context, userID, entitlement string, at time.Time) (*models.Entitlement, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	return r.GetLatestFiniteActiveByTenantSubject(ctx, tsid, entitlement, at)
}

func (r *EntitlementRepo) GetLatestFiniteActiveByTenantSubject(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (*models.Entitlement, error) {
	row, err := r.db.Gen(ctx).GetLatestFiniteActiveEntitlement(ctx, gen.GetLatestFiniteActiveEntitlementParams{
		TenantID:        tenant.FromContextOrDefault(ctx).UUID(),
		TenantSubjectID: tenantSubjectID,
		Entitlement:     entitlement,
		At:              at,
	})
	if err != nil {
		return nil, err
	}
	return entitlementFromGen(row), nil
}

func (r *EntitlementRepo) Insert(ctx context.Context, entitlement *models.Entitlement) error {
	// Validate that end_at > start_at if end_at is provided (non-indefinite entitlement)
	if entitlement.EndAt != nil && !entitlement.EndAt.After(entitlement.StartAt) {
		return fmt.Errorf("invalid entitlement: end_at (%v) must be after start_at (%v)", entitlement.EndAt, entitlement.StartAt)
	}

	// Stamp the resolved tenant (issue #223) when the caller did not set one,
	// so new rows are tenant-scoped consistently with reads. Defaults to the
	// default tenant for single-tenant / self-hosted runs.
	if (entitlement.TenantID == uuid.UUID{}) {
		entitlement.TenantID = tenant.FromContextOrDefault(ctx).UUID()
	}

	id, err := r.db.Gen(ctx).CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:              entitlement.ID,
		TenantID:        entitlement.TenantID,
		TenantSubjectID: entitlement.TenantSubjectID,
		Entitlement:     entitlement.Entitlement,
		StartAt:         entitlement.StartAt,
		EndAt:           entitlement.EndAt,
		SourceID:        entitlement.SourceID,
		SourceType:      string(entitlement.SourceType),
		RevokedAt:       entitlement.RevokedAt,
		RevokeReason:    revokeReasonPtr(entitlement.RevokeReason),
		CreatedAt:       entitlement.CreatedAt,
		UpdatedAt:       entitlement.UpdatedAt,
	})
	if err != nil {
		return err
	}
	entitlement.ID = id
	return nil
}

func (r *EntitlementRepo) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	return r.db.Gen(ctx).ListActiveEntitlementNames(ctx, gen.ListActiveEntitlementNamesParams{
		TenantSubjectID: tsid,
		At:              at,
	})
}

func (r *EntitlementRepo) ListActiveEntitlementsByTenantSubject(ctx context.Context, tenantSubjectID uuid.UUID, at time.Time) ([]string, error) {
	return r.db.Gen(ctx).ListActiveEntitlementNamesTenant(ctx, gen.ListActiveEntitlementNamesTenantParams{
		TenantID:        tenant.FromContextOrDefault(ctx).UUID(),
		TenantSubjectID: tenantSubjectID,
		At:              at,
	})
}

func (r *EntitlementRepo) ListActiveRecords(ctx context.Context, userID string, at time.Time) ([]models.Entitlement, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListActiveEntitlementRecords(ctx, gen.ListActiveEntitlementRecordsParams{
		TenantSubjectID: tsid,
		At:              at,
	})
	if err != nil {
		return nil, err
	}
	return entitlementsFromGen(rows), nil
}

func (r *EntitlementRepo) ListActiveRecordsByTenantSubject(ctx context.Context, tenantSubjectID uuid.UUID, at time.Time) ([]models.Entitlement, error) {
	rows, err := r.db.Gen(ctx).ListActiveEntitlementRecordsTenant(ctx, gen.ListActiveEntitlementRecordsTenantParams{
		TenantID:        tenant.FromContextOrDefault(ctx).UUID(),
		TenantSubjectID: tenantSubjectID,
		At:              at,
	})
	if err != nil {
		return nil, err
	}
	return entitlementsFromGen(rows), nil
}

func (r *EntitlementRepo) ListDistinctEntitlementNamesBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID) ([]string, error) {
	return r.db.Gen(ctx).ListDistinctEntitlementNamesBySource(ctx, gen.ListDistinctEntitlementNamesBySourceParams{
		SourceType: string(sourceType),
		SourceID:   sourceID,
	})
}

// EndActiveBySubscription ends entitlements for a subscription.
// If reason is nil, only end_at is set (for period-end expirations).
// If reason is provided, revoked_at and revoke_reason are also set (for immediate revocations).
// Returns an error if any entitlement would have end_at <= start_at (zero or negative duration).
// The now parameter is used for updated_at and revoked_at timestamps to support mock clocks in tests.
func (r *EntitlementRepo) EndActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time, now time.Time, reason *models.EntitlementRevokeReason) error {
	q := r.db.Gen(ctx)
	invalidCount, err := q.CountInvalidEndBySubscription(ctx, gen.CountInvalidEndBySubscriptionParams{
		SourceID: subscriptionID,
		EndAt:    endAt,
	})
	if err != nil {
		return fmt.Errorf("failed to check entitlement validity: %w", err)
	}
	if invalidCount > 0 {
		return fmt.Errorf("cannot set end_at to %v: %d entitlement(s) have start_at >= end_at (zero or negative duration)", endAt, invalidCount)
	}

	return q.EndActiveEntitlementsBySubscription(ctx, gen.EndActiveEntitlementsBySubscriptionParams{
		SourceID:     subscriptionID,
		EndAt:        endAt,
		Now:          now,
		SetRevoked:   reason != nil,
		RevokeReason: revokeReasonPtr(reason),
	})
}

// ExtendActiveBySubscription extends active entitlements for a subscription to endAt.
// It only updates rows whose end_at is NULL or before endAt, and will never shorten a window.
func (r *EntitlementRepo) ExtendActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time, now time.Time) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Fetch all subscription entitlements that would be extended, then shift any following
		// scheduled windows forward by the same delta (per user+entitlement) to avoid overlaps.
		//
		// This keeps the entitlement timeline gapless for the affected entitlement key and avoids
		// double-access from overlapping scheduled windows.
		q := gen.New(tx)
		rows, err := q.ListExtendableSubscriptionEntitlements(ctx, gen.ListExtendableSubscriptionEntitlementsParams{
			SourceID: subscriptionID,
			EndAt:    endAt,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		for _, row := range rows {
			ent := entitlementFromGen(row)
			if ent.EndAt == nil || ent.EndAt.IsZero() {
				continue
			}
			oldEnd := ent.EndAt.UTC()
			newEnd := endAt.UTC()
			if !newEnd.After(oldEnd) {
				continue
			}

			// Validate: do not produce end_at <= start_at
			if !newEnd.After(ent.StartAt) {
				return fmt.Errorf("cannot extend end_at to %v: entitlement start_at=%v would be >= end_at", newEnd, ent.StartAt)
			}

			if err := LockEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement); err != nil {
				return err
			}

			// Shift the following windows forward FIRST to open the gap. Extending
			// the subscription row to newEnd before shifting would transiently
			// overlap the next (not-yet-shifted) window, and entitlements_no_overlap
			// is an IMMEDIATE (non-deferrable) exclusion constraint checked per
			// statement — so it must never be violated mid-transaction.
			delta := newEnd.Sub(oldEnd)
			if err := ShiftEntitlementTimeline(ctx, tx, ent.TenantSubjectID.String(), ent.Entitlement, oldEnd, delta, now, []uuid.UUID{ent.ID}); err != nil {
				return err
			}

			// Extend the subscription's entitlement row.
			if err := q.UpdateEntitlementEndAtIfMatch(ctx, gen.UpdateEntitlementEndAtIfMatchParams{
				ID:       ent.ID,
				NewEndAt: newEnd,
				Now:      now,
				OldEndAt: oldEnd,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResumeBySubscription clears end_at for active entitlements that were scheduled to end.
// This is used when a user resumes a cancellation before the current period ends.
func (r *EntitlementRepo) ResumeBySubscription(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	return r.db.Gen(ctx).ResumeEntitlementsBySubscription(ctx, gen.ResumeEntitlementsBySubscriptionParams{
		SourceID: subscriptionID,
		Now:      now,
	})
}

// EndActiveByPayment revokes active one-off entitlements for a payment and removes future windows.
// The now parameter is used for updated_at and revoked_at timestamps to support mock clocks in tests.
func (r *EntitlementRepo) EndActiveByPayment(ctx context.Context, paymentID uuid.UUID, endAt time.Time, now time.Time, reason *models.EntitlementRevokeReason) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.SoftDeleteFutureOneOffEntitlements(ctx, gen.SoftDeleteFutureOneOffEntitlementsParams{
			SourceID: paymentID,
			Now:      now,
			EndAt:    endAt,
		}); err != nil {
			return err
		}
		return q.RevokeActiveOneOffEntitlements(ctx, gen.RevokeActiveOneOffEntitlementsParams{
			SourceID:     paymentID,
			EndAt:        endAt,
			Now:          now,
			RevokeReason: revokeReasonPtr(reason),
		})
	})
}

func (r *EntitlementRepo) ExistsBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID, entitlement string) (bool, error) {
	return r.db.Gen(ctx).EntitlementExistsBySource(ctx, gen.EntitlementExistsBySourceParams{
		SourceType:  string(sourceType),
		SourceID:    sourceID,
		Entitlement: entitlement,
	})
}

func (r *EntitlementRepo) ListByUser(ctx context.Context, userID string) ([]models.Entitlement, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListEntitlementsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return entitlementsFromGen(rows), nil
}

// GetByID retrieves an entitlement by its ID
func (r *EntitlementRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Entitlement, error) {
	row, err := r.db.Gen(ctx).GetEntitlementByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return entitlementFromGen(row), nil
}

// RevokeByID immediately revokes an entitlement by setting revoked_at and revoke_reason
// The now parameter is used for revoked_at and updated_at timestamps to support mock clocks in tests.
func (r *EntitlementRepo) RevokeByID(ctx context.Context, id uuid.UUID, now time.Time, reason models.EntitlementRevokeReason) error {
	rows, err := r.db.Gen(ctx).RevokeEntitlementByID(ctx, gen.RevokeEntitlementByIDParams{
		ID:           id,
		Now:          now,
		RevokeReason: string(reason),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return errors.New("entitlement not found or already revoked")
	}
	return nil
}

// RevokeBySubscriptionAndName revokes a specific entitlement by subscription ID and entitlement name.
// Used during downgrades to revoke entitlements that the new tier doesn't include.
// Not finding an entitlement to revoke is not an error - it may have already been
// revoked or never existed for this subscription.
func (r *EntitlementRepo) RevokeBySubscriptionAndName(ctx context.Context, subscriptionID uuid.UUID, entitlement string, revokeAt time.Time, reason models.EntitlementRevokeReason) error {
	_, err := r.db.Gen(ctx).RevokeEntitlementBySubscriptionAndName(ctx, gen.RevokeEntitlementBySubscriptionAndNameParams{
		SourceID:     subscriptionID,
		Entitlement:  entitlement,
		RevokeAt:     revokeAt,
		RevokeReason: string(reason),
	})
	return err
}
