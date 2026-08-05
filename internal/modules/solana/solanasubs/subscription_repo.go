package solanasubs

import (
	"context"
	"errors"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SolanaSubscriptionRepo persists the on-chain state of recurring Solana
// subscriptions (issue #255).
type SolanaSubscriptionRepo struct {
	db *db.DB
}

func NewSolanaSubscriptionRepo(d *db.DB) *SolanaSubscriptionRepo {
	return &SolanaSubscriptionRepo{db: d}
}

func solanaSubscriptionFromGen(s gen.OpenrailsSolanaSubscription) *models.SolanaSubscription {
	return &models.SolanaSubscription{
		ID:                       s.ID,
		MerchantID:               s.MerchantID,
		SubscriptionID:           s.SubscriptionID,
		SubscriberWallet:         s.SubscriberWallet,
		AuthorityPDA:             s.AuthorityPda,
		SubscriptionPDA:          s.SubscriptionPda,
		PlanPDA:                  s.PlanPda,
		MerchantAddress:          s.MerchantAddress,
		Mint:                     s.Mint,
		PlanCreatedAtFingerprint: s.PlanCreatedAtFingerprint,
		LastPulledPeriodStart:    s.LastPulledPeriodStart,
		LastSignature:            s.LastSignature,
		NextPullAt:               s.NextPullAt,
		Status:                   s.Status,
		CreatedAt:                s.CreatedAt,
		UpdatedAt:                s.UpdatedAt,
	}
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
	return r.db.Gen(ctx).UpsertSolanaSubscription(ctx, gen.UpsertSolanaSubscriptionParams{
		ID:                       s.ID,
		MerchantID:               s.MerchantID,
		SubscriptionID:           s.SubscriptionID,
		SubscriberWallet:         s.SubscriberWallet,
		AuthorityPda:             s.AuthorityPDA,
		SubscriptionPda:          s.SubscriptionPDA,
		PlanPda:                  s.PlanPDA,
		MerchantAddress:          s.MerchantAddress,
		Mint:                     s.Mint,
		PlanCreatedAtFingerprint: s.PlanCreatedAtFingerprint,
		LastPulledPeriodStart:    s.LastPulledPeriodStart,
		LastSignature:            s.LastSignature,
		NextPullAt:               s.NextPullAt,
		Status:                   s.Status,
		CreatedAt:                s.CreatedAt,
		UpdatedAt:                s.UpdatedAt,
	})
}

// GetBySubscriptionPDA returns the row for an on-chain subscription PDA.
func (r *SolanaSubscriptionRepo) GetBySubscriptionPDA(ctx context.Context, pda string) (*models.SolanaSubscription, error) {
	row, err := r.db.Gen(ctx).GetSolanaSubscriptionByPDA(ctx, pda)
	if err != nil {
		return nil, err
	}
	return solanaSubscriptionFromGen(row), nil
}

// GetBySubscriptionID returns the row linked to a lifecycle subscription.
func (r *SolanaSubscriptionRepo) GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error) {
	row, err := r.db.Gen(ctx).GetSolanaSubscriptionBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		return nil, err
	}
	return solanaSubscriptionFromGen(row), nil
}

// ListDue returns active subscriptions whose next_pull_at is at/before `now`,
// ordered by merchant so the worker can load each merchant's signer once. `limit`
// caps the batch (0 = no limit).
func (r *SolanaSubscriptionRepo) ListDue(ctx context.Context, now time.Time, limit int) ([]*models.SolanaSubscription, error) {
	if _, ok := merchant.FromContext(ctx); ok {
		return r.listDueScoped(ctx, now, limit)
	}
	merchantIDs, err := r.activeMerchantIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.SolanaSubscription, 0)
	for _, id := range merchantIDs {
		if limit > 0 && len(out) >= limit {
			break
		}
		remaining := limit
		if remaining > 0 {
			remaining -= len(out)
		}
		err := r.db.RunInMerchantConn(merchant.WithID(ctx, merchant.ID(id)), func(ctx context.Context) error {
			rows, err := r.listDueScoped(ctx, now, remaining)
			if err != nil {
				return err
			}
			out = append(out, rows...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *SolanaSubscriptionRepo) listDueScoped(ctx context.Context, now time.Time, limit int) ([]*models.SolanaSubscription, error) {
	limit32, _ := safecast.Convert[int32](limit)
	rows, err := r.db.Gen(ctx).ListDueSolanaSubscriptions(ctx, gen.ListDueSolanaSubscriptionsParams{
		Now:       now.UTC(),
		PageLimit: limit32,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*models.SolanaSubscription, 0, len(rows))
	for _, row := range rows {
		sub := solanaSubscriptionFromGen(row.OpenrailsSolanaSubscription)
		sub.PspID = row.PspID
		out = append(out, sub)
	}
	return out, nil
}

// AdvanceAfterPull records a confirmed pull: stamps the signature + period start
// and moves next_pull_at forward. Source of truth for "paid through" is the
// confirmed pull, not wall-clock.
func (r *SolanaSubscriptionRepo) AdvanceAfterPull(ctx context.Context, id uuid.UUID, periodStart time.Time, signature string, nextPullAt time.Time) error {
	ps := periodStart.UTC()
	return r.db.Gen(ctx).AdvanceSolanaSubscriptionAfterPull(ctx, gen.AdvanceSolanaSubscriptionAfterPullParams{
		ID:                    id,
		LastPulledPeriodStart: &ps,
		LastSignature:         &signature,
		NextPullAt:            nextPullAt.UTC(),
		UpdatedAt:             time.Now().UTC(),
	})
}

// SetNextPullAt reschedules the next pull without recording a payment — used on
// a failed crank to align the next attempt with the dunning retry cadence (so
// the hourly worker doesn't re-attempt + re-fail every hour, collapsing the
// multi-day dunning window).
func (r *SolanaSubscriptionRepo) SetNextPullAt(ctx context.Context, id uuid.UUID, nextPullAt time.Time) error {
	return r.db.Gen(ctx).SetSolanaSubscriptionNextPullAt(ctx, gen.SetSolanaSubscriptionNextPullAtParams{
		ID:         id,
		NextPullAt: nextPullAt.UTC(),
		UpdatedAt:  time.Now().UTC(),
	})
}

// MerchantWallet is a distinct (merchant, cranker wallet) pair with active subs.
type MerchantWallet struct {
	MerchantID      uuid.UUID
	MerchantAddress string
}

// ListActiveMerchantWallets returns the distinct cranker (merchant) wallets that
// have at least one active subscription — the wallets whose SOL float the
// gas-alert worker monitors (#258).
func (r *SolanaSubscriptionRepo) ListActiveMerchantWallets(ctx context.Context) ([]MerchantWallet, error) {
	if _, ok := merchant.FromContext(ctx); ok {
		return r.listActiveMerchantWalletsScoped(ctx)
	}
	merchantIDs, err := r.activeMerchantIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MerchantWallet, 0)
	for _, id := range merchantIDs {
		err := r.db.RunInMerchantConn(merchant.WithID(ctx, merchant.ID(id)), func(ctx context.Context) error {
			rows, err := r.listActiveMerchantWalletsScoped(ctx)
			if err != nil {
				return err
			}
			out = append(out, rows...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *SolanaSubscriptionRepo) listActiveMerchantWalletsScoped(ctx context.Context) ([]MerchantWallet, error) {
	rows, err := r.db.Gen(ctx).ListActiveSolanaMerchantWallets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]MerchantWallet, 0, len(rows))
	for _, row := range rows {
		out = append(out, MerchantWallet{MerchantID: row.MerchantID, MerchantAddress: row.MerchantAddress})
	}
	return out, nil
}

// ListActiveWithSignature returns active subscriptions that have recorded at
// least one confirmed pull (last_signature set) — the rows the reconciliation
// worker cross-checks against openrails.payments (#258). `limit` caps the batch
// (0 = no limit).
func (r *SolanaSubscriptionRepo) ListActiveWithSignature(ctx context.Context, limit int) ([]*models.SolanaSubscription, error) {
	if _, ok := merchant.FromContext(ctx); ok {
		return r.listActiveWithSignatureScoped(ctx, limit)
	}
	merchantIDs, err := r.activeMerchantIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.SolanaSubscription, 0)
	for _, id := range merchantIDs {
		if limit > 0 && len(out) >= limit {
			break
		}
		remaining := limit
		if remaining > 0 {
			remaining -= len(out)
		}
		err := r.db.RunInMerchantConn(merchant.WithID(ctx, merchant.ID(id)), func(ctx context.Context) error {
			rows, err := r.listActiveWithSignatureScoped(ctx, remaining)
			if err != nil {
				return err
			}
			out = append(out, rows...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *SolanaSubscriptionRepo) listActiveWithSignatureScoped(ctx context.Context, limit int) ([]*models.SolanaSubscription, error) {
	limit32, _ := safecast.Convert[int32](limit)
	rows, err := r.db.Gen(ctx).ListActiveSolanaSubscriptionsWithSignature(ctx, limit32)
	if err != nil {
		return nil, err
	}
	out := make([]*models.SolanaSubscription, 0, len(rows))
	for _, row := range rows {
		out = append(out, solanaSubscriptionFromGen(row))
	}
	return out, nil
}

// SetStatus transitions the on-chain record's lifecycle (cancelled/expired).
func (r *SolanaSubscriptionRepo) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	return r.db.Gen(ctx).SetSolanaSubscriptionStatus(ctx, gen.SetSolanaSubscriptionStatusParams{
		ID:        id,
		Status:    status,
		UpdatedAt: time.Now().UTC(),
	})
}

func (r *SolanaSubscriptionRepo) activeMerchantIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.db.Qx(ctx).Query(ctx, `
		SELECT id FROM openrails.merchants
		 WHERE status = 'active' AND deleted_at IS NULL
		 ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}
