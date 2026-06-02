package repo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

// SolanaSubscriptionRepo persists the on-chain state of recurring Solana
// subscriptions (issue #255).
type SolanaSubscriptionRepo struct {
	db *db.DB
}

func NewSolanaSubscriptionRepo(d *db.DB) *SolanaSubscriptionRepo {
	return &SolanaSubscriptionRepo{db: d}
}

// Upsert inserts the row or, if one already exists for the subscription PDA,
// refreshes its mutable identity/state. Idempotent on subscription_pda so the
// enroll-confirmation poller can run at-least-once safely.
func (r *SolanaSubscriptionRepo) Upsert(ctx context.Context, s *models.SolanaSubscription) error {
	if s == nil {
		return errors.New("solana subscription is nil")
	}
	now := time.Now().UTC()
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Status == "" {
		s.Status = models.SolanaSubscriptionActive
	}

	_, err := r.db.Q(ctx).NewInsert().
		Model(s).
		On("CONFLICT (subscription_pda) DO UPDATE").
		Set("subscription_id = EXCLUDED.subscription_id").
		Set("next_pull_at = EXCLUDED.next_pull_at").
		Set("status = EXCLUDED.status").
		Set("plan_created_at_fingerprint = EXCLUDED.plan_created_at_fingerprint").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

// GetBySubscriptionPDA returns the row for an on-chain subscription PDA.
func (r *SolanaSubscriptionRepo) GetBySubscriptionPDA(ctx context.Context, pda string) (*models.SolanaSubscription, error) {
	s := new(models.SolanaSubscription)
	if err := r.db.Q(ctx).NewSelect().Model(s).Where("subscription_pda = ?", pda).Scan(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// GetBySubscriptionID returns the row linked to a lifecycle subscription.
func (r *SolanaSubscriptionRepo) GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error) {
	s := new(models.SolanaSubscription)
	if err := r.db.Q(ctx).NewSelect().Model(s).Where("subscription_id = ?", subscriptionID).Scan(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// ListDue returns active subscriptions whose next_pull_at is at/before `now`,
// ordered by tenant so the worker can load each tenant's signer once. `limit`
// caps the batch (0 = no limit).
func (r *SolanaSubscriptionRepo) ListDue(ctx context.Context, now time.Time, limit int) ([]*models.SolanaSubscription, error) {
	var rows []*models.SolanaSubscription
	q := r.db.Q(ctx).NewSelect().
		Model(&rows).
		Where("status = ?", models.SolanaSubscriptionActive).
		Where("next_pull_at <= ?", now.UTC()).
		Order("tenant_id ASC", "next_pull_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// AdvanceAfterPull records a confirmed pull: stamps the signature + period start
// and moves next_pull_at forward. Source of truth for "paid through" is the
// confirmed pull, not wall-clock.
func (r *SolanaSubscriptionRepo) AdvanceAfterPull(ctx context.Context, id uuid.UUID, periodStart time.Time, signature string, nextPullAt time.Time) error {
	ps := periodStart.UTC()
	_, err := r.db.Q(ctx).NewUpdate().
		Model((*models.SolanaSubscription)(nil)).
		Set("last_pulled_period_start = ?", ps).
		Set("last_signature = ?", signature).
		Set("next_pull_at = ?", nextPullAt.UTC()).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

// MerchantWallet is a distinct (tenant, cranker wallet) pair with active subs.
type MerchantWallet struct {
	TenantID        uuid.UUID `bun:"tenant_id"`
	MerchantAddress string    `bun:"merchant_address"`
}

// ListActiveMerchantWallets returns the distinct cranker (merchant) wallets that
// have at least one active subscription — the wallets whose SOL float the
// gas-alert worker monitors (#258).
func (r *SolanaSubscriptionRepo) ListActiveMerchantWallets(ctx context.Context) ([]MerchantWallet, error) {
	var rows []MerchantWallet
	err := r.db.Q(ctx).NewSelect().
		Model((*models.SolanaSubscription)(nil)).
		ColumnExpr("DISTINCT tenant_id, merchant_address").
		Where("status = ?", models.SolanaSubscriptionActive).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// ListActiveWithSignature returns active subscriptions that have recorded at
// least one confirmed pull (last_signature set) — the rows the reconciliation
// worker cross-checks against billing.payments (#258). `limit` caps the batch
// (0 = no limit).
func (r *SolanaSubscriptionRepo) ListActiveWithSignature(ctx context.Context, limit int) ([]*models.SolanaSubscription, error) {
	var rows []*models.SolanaSubscription
	q := r.db.Q(ctx).NewSelect().
		Model(&rows).
		Where("status = ?", models.SolanaSubscriptionActive).
		Where("last_signature IS NOT NULL").
		Where("last_signature <> ''").
		Order("updated_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// SetStatus transitions the on-chain record's lifecycle (cancelled/expired).
func (r *SolanaSubscriptionRepo) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := r.db.Q(ctx).NewUpdate().
		Model((*models.SolanaSubscription)(nil)).
		Set("status = ?", status).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}
