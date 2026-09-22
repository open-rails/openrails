package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// RefuseOwnedRebillTerms must run after locking the subscription in the same
// transaction as the proposed mutation. Admission and completion use that same
// lock, so neither a new accepted attempt nor its terminal outcome can race this
// decision. No operation lock is acquired before the subscription lock.
func RefuseOwnedRebillTerms(ctx context.Context, d *db.DB, sub *models.Subscription) error {
	if d == nil || d.Pool() != nil || sub == nil {
		return fmt.Errorf("%w: ownership check requires a locked subscription", ErrRebillTermsCommitted)
	}
	engine, err := d.Gen(ctx).GetUnresolvedSubscriptionCollection(ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err == nil {
		if _, err := DecodeSubscriptionCollectionPayload(engine); err != nil {
			return fmt.Errorf("%w: invalid engine owner: %v", ErrRebillTermsCommitted, err)
		}
		return ErrRebillTermsCommitted
	}
	if !db.IsNotFound(err) {
		return err
	}
	rows, err := d.Gen(ctx).ListRebillTermOwners(ctx, gen.ListRebillTermOwnersParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err != nil {
		return err
	}
	for _, row := range rows {
		accepted, err := DecodeManualRebillPayload(row)
		if err != nil {
			return fmt.Errorf("%w: accepted rebill terms are invalid: %v", ErrRebillTermsCommitted, err)
		}
		switch row.Status {
		case "pending", "in_flight", "unknown_needs_verify", "failed_retryable":
			return ErrRebillTermsCommitted
		}
		// scheduled_price_id is a reusable target, not a quote identity. A terminal
		// historical A->B operation does not own another A->B in a later period.
		// Unique reprice rows instead remain owned while that exact row is pending.
		if accepted.Renewal.ScheduledPriceID != nil && (sub.CurrentPeriodEndsAt == nil ||
			!sub.CurrentPeriodEndsAt.Equal(accepted.Renewal.PeriodStart) || sub.PriceID != accepted.Renewal.FromPriceID || sub.ProductID != accepted.Renewal.FromProductID) {
			continue
		}
		// Only the canonical handler's terminal pre-preparation release proves that
		// no provider terms changed. A decline, lease expiry or absent progress on
		// unresolved work does not provide that proof.
		if row.Status != "failed_terminal" {
			return ErrRebillTermsCommitted
		}
		var evidence map[string]json.RawMessage
		if err := json.Unmarshal(row.ResultEvidence, &evidence); err != nil {
			return ErrRebillTermsCommitted
		}
		for _, key := range []string{"rebill_preparation", "submitted_at", "qualified_receipt", "rebill_decline"} {
			if _, exists := evidence[key]; exists {
				return ErrRebillTermsCommitted
			}
		}
		var notExecuted bool
		if err := json.Unmarshal(evidence["not_executed"], &notExecuted); err != nil || !notExecuted {
			return ErrRebillTermsCommitted
		}
	}
	return nil
}

// SchedulePriceChange updates only a freshly locked subscription. Checkout's
// preflight model cannot overwrite a renewal that committed in the meantime.
func (r *SubscriptionRepo) SchedulePriceChange(ctx context.Context, id, expectedPrice, targetPrice uuid.UUID) (*models.Subscription, error) {
	var sub *models.Subscription
	err := r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := r.db.NewWithPgxTx(tx)
		repo := NewSubscriptionRepo(d)
		var err error
		sub, err = repo.GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
			return err
		}
		if sub.PriceID != expectedPrice || sub.ScheduledPriceID != nil {
			return ErrRepriceAlreadyScheduled
		}
		sub.ScheduledPriceID = &targetPrice
		return repo.Update(ctx, sub)
	})
	return sub, err
}
