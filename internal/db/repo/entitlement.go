package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/google/uuid"
)

type EntitlementRepo struct {
	db *db.DB
}

func NewEntitlementRepo(d *db.DB) *EntitlementRepo { return &EntitlementRepo{db: d} }

func (r *EntitlementRepo) IsEntitled(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	q := r.db.GetDB().NewSelect().
		Model((*models.Entitlement)(nil)).
		Where("ent.user_id = ?", userID).
		Where("ent.entitlement = ?", entitlement).
		Where("ent.start_at <= ?", at).
		Where("(ent.end_at IS NULL OR ent.end_at > ?)", at).
		Where("ent.revoked_at IS NULL")
	return q.Exists(ctx)
}

func (r *EntitlementRepo) HasActiveIndefinite(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	q := r.db.GetDB().NewSelect().
		Model((*models.Entitlement)(nil)).
		Where("ent.user_id = ?", userID).
		Where("ent.entitlement = ?", entitlement).
		Where("ent.revoked_at IS NULL AND ent.end_at IS NULL").
		Where("ent.start_at <= ?", at)
	return q.Exists(ctx)
}

func (r *EntitlementRepo) GetLatestActive(ctx context.Context, userID, entitlement string) (*models.Entitlement, error) {
	var ent models.Entitlement
	err := r.db.GetDB().NewSelect().
		Model(&ent).
		Where("ent.user_id = ? AND ent.entitlement = ?", userID, entitlement).
		Where("ent.revoked_at IS NULL").
		OrderExpr("ent.start_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return &ent, nil
}

func (r *EntitlementRepo) GetLatestFiniteActive(ctx context.Context, userID, entitlement string, at time.Time) (*models.Entitlement, error) {
	var ent models.Entitlement
	err := r.db.GetDB().NewSelect().
		Model(&ent).
		Where("ent.user_id = ? AND ent.entitlement = ?", userID, entitlement).
		Where("ent.revoked_at IS NULL").
		Where("ent.end_at IS NOT NULL").
		Where("ent.start_at <= ?", at).
		Where("ent.end_at > ?", at).
		OrderExpr("ent.end_at DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return &ent, nil
}

func (r *EntitlementRepo) Insert(ctx context.Context, entitlement *models.Entitlement) error {
	// Validate that end_at > start_at if end_at is provided (non-indefinite entitlement)
	if entitlement.EndAt != nil && !entitlement.EndAt.After(entitlement.StartAt) {
		return fmt.Errorf("invalid entitlement: end_at (%v) must be after start_at (%v)", entitlement.EndAt, entitlement.StartAt)
	}

	res, err := r.db.GetDB().NewInsert().Model(entitlement).Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *EntitlementRepo) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	var out []string
	if err := r.db.GetDB().NewSelect().
		Model((*models.Entitlement)(nil)).
		ColumnExpr("DISTINCT ent.entitlement").
		Where("ent.user_id = ?", userID).
		Where("ent.start_at <= ?", at).
		Where("(ent.end_at IS NULL OR ent.end_at > ?)", at).
		Where("ent.revoked_at IS NULL").
		Scan(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *EntitlementRepo) ListActiveRecords(ctx context.Context, userID string, at time.Time) ([]models.Entitlement, error) {
	ents := []models.Entitlement{}
	if err := r.db.GetDB().NewSelect().
		Model(&ents).
		Where("ent.user_id = ?", userID).
		Where("ent.revoked_at IS NULL").
		Where("ent.start_at <= ?", at).
		Where("(ent.end_at IS NULL OR ent.end_at > ?)", at).
		OrderExpr("ent.start_at ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return ents, nil
}

// EndActiveBySubscription ends entitlements for a subscription.
// If reason is nil, only end_at is set (for period-end expirations).
// If reason is provided, revoked_at and revoke_reason are also set (for immediate revocations).
// Returns an error if any entitlement would have end_at <= start_at (zero or negative duration).
// The now parameter is used for updated_at and revoked_at timestamps to support mock clocks in tests.
func (r *EntitlementRepo) EndActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time, now time.Time, reason *models.EntitlementRevokeReason) error {
	// First, check if any entitlements would violate the start_at < end_at constraint
	var invalidCount int
	err := r.db.GetDB().NewSelect().
		Model((*models.Entitlement)(nil)).
		ColumnExpr("COUNT(*)").
		Where("ent.source_type = ?", models.EntitlementSourceSubscription).
		Where("ent.source_id = ?", subscriptionID).
		Where("ent.end_at IS NULL").
		Where("ent.start_at >= ?", endAt).
		Scan(ctx, &invalidCount)
	if err != nil {
		return fmt.Errorf("failed to check entitlement validity: %w", err)
	}
	if invalidCount > 0 {
		return fmt.Errorf("cannot set end_at to %v: %d entitlement(s) have start_at >= end_at (zero or negative duration)", endAt, invalidCount)
	}

	q := r.db.GetDB().NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("end_at = ?", endAt).
		Set("updated_at = ?", now).
		Where("ent.source_type = ?", models.EntitlementSourceSubscription).
		Where("ent.source_id = ?", subscriptionID).
		Where("ent.end_at IS NULL")

	// Only set revoked_at and revoke_reason if a reason is provided (immediate revocation)
	if reason != nil {
		q = q.Set("revoked_at = ?", now).
			Set("revoke_reason = ?", reason)
	}

	_, err = q.Exec(ctx)
	return err
}

// ResumeBySubscription clears end_at for active entitlements that were scheduled to end.
// This is used when a user resumes a cancellation before the current period ends.
func (r *EntitlementRepo) ResumeBySubscription(ctx context.Context, subscriptionID uuid.UUID, now time.Time) error {
	_, err := r.db.GetDB().NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("end_at = NULL").
		Set("updated_at = ?", now).
		Where("ent.source_type = ?", models.EntitlementSourceSubscription).
		Where("ent.source_id = ?", subscriptionID).
		Where("ent.revoked_at IS NULL").
		Where("ent.end_at IS NOT NULL").
		Where("ent.end_at > ?", now).
		Exec(ctx)
	return err
}

// EndActiveByPayment ends entitlements for a one-off payment.
// Returns an error if any entitlement would have end_at <= start_at (zero or negative duration).
// The now parameter is used for updated_at and revoked_at timestamps to support mock clocks in tests.
func (r *EntitlementRepo) EndActiveByPayment(ctx context.Context, paymentID uuid.UUID, endAt time.Time, now time.Time, reason *models.EntitlementRevokeReason) error {
	// First, check if any entitlements would violate the start_at < end_at constraint
	var invalidCount int
	err := r.db.GetDB().NewSelect().
		Model((*models.Entitlement)(nil)).
		ColumnExpr("COUNT(*)").
		Where("ent.source_type = ?", models.EntitlementSourceOneOff).
		Where("ent.source_id = ?", paymentID).
		Where("ent.end_at IS NULL").
		Where("ent.start_at >= ?", endAt).
		Scan(ctx, &invalidCount)
	if err != nil {
		return fmt.Errorf("failed to check entitlement validity: %w", err)
	}
	if invalidCount > 0 {
		return fmt.Errorf("cannot set end_at to %v: %d entitlement(s) have start_at >= end_at (zero or negative duration)", endAt, invalidCount)
	}

	_, err = r.db.GetDB().NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("end_at = ?", endAt).
		Set("revoked_at = ?", now).
		Set("revoke_reason = ?", reason).
		Set("updated_at = ?", now).
		Where("ent.source_type = ?", models.EntitlementSourceOneOff).
		Where("ent.source_id = ?", paymentID).
		Where("ent.end_at IS NULL").
		Exec(ctx)
	return err
}

func (r *EntitlementRepo) ExistsBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID, entitlement string) (bool, error) {
	return r.db.GetDB().NewSelect().
		Model((*models.Entitlement)(nil)).
		Where("ent.source_type = ?", sourceType).
		Where("ent.source_id = ?", sourceID).
		Where("ent.entitlement = ?", entitlement).
		Where("ent.revoked_at IS NULL").
		Exists(ctx)
}

func (r *EntitlementRepo) ListByUser(ctx context.Context, userID string) ([]models.Entitlement, error) {
	ents := []models.Entitlement{}
	if err := r.db.GetDB().NewSelect().
		Model(&ents).
		Where("ent.user_id = ?", userID).
		OrderExpr("ent.start_at DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return ents, nil
}

// GetByID retrieves an entitlement by its ID
func (r *EntitlementRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Entitlement, error) {
	var ent models.Entitlement
	err := r.db.GetDB().NewSelect().
		Model(&ent).
		Where("ent.id = ?", id).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return &ent, nil
}

// RevokeByID immediately revokes an entitlement by setting revoked_at and revoke_reason
// The now parameter is used for revoked_at and updated_at timestamps to support mock clocks in tests.
func (r *EntitlementRepo) RevokeByID(ctx context.Context, id uuid.UUID, now time.Time, reason models.EntitlementRevokeReason) error {
	res, err := r.db.GetDB().NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("revoked_at = ?", now).
		Set("revoke_reason = ?", reason).
		Set("updated_at = ?", now).
		Where("ent.id = ?", id).
		Where("ent.revoked_at IS NULL"). // Only revoke if not already revoked
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
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
func (r *EntitlementRepo) RevokeBySubscriptionAndName(ctx context.Context, subscriptionID uuid.UUID, entitlement string, revokeAt time.Time, reason models.EntitlementRevokeReason) error {
	res, err := r.db.GetDB().NewUpdate().
		Model((*models.Entitlement)(nil)).
		Set("revoked_at = ?", revokeAt).
		Set("revoke_reason = ?", reason).
		Set("end_at = ?", revokeAt). // Also set end_at to terminate access
		Set("updated_at = ?", revokeAt).
		Where("ent.source_type = ?", models.EntitlementSourceSubscription).
		Where("ent.source_id = ?", subscriptionID).
		Where("ent.entitlement = ?", entitlement).
		Where("ent.revoked_at IS NULL"). // Only revoke if not already revoked
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		// Not finding an entitlement to revoke is not an error - it may have already been revoked
		// or never existed for this subscription
		return nil
	}
	return nil
}
