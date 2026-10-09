package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"

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
		if _, err := DecodeManualRebillPayload(row); err != nil {
			return fmt.Errorf("%w: accepted rebill terms are invalid: %v", ErrRebillTermsCommitted, err)
		}
		switch row.Status {
		case "pending", "in_flight", "unknown_needs_verify", "failed_retryable":
			return ErrRebillTermsCommitted
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

// refuseSubmittedRenewal refuses a lifecycle change while the subscription's
// unresolved renewal collection has crossed its submission fence. Run it on
// the locked subscription in the mutation's transaction.
func refuseSubmittedRenewal(ctx context.Context, d *db.DB, sub *models.Subscription) error {
	row, err := d.Gen(ctx).GetUnresolvedSubscriptionCollection(ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if db.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var evidence map[string]json.RawMessage
	if len(row.ResultEvidence) > 0 {
		if err := json.Unmarshal(row.ResultEvidence, &evidence); err != nil {
			return ErrRenewalInProgress
		}
	}
	if _, submitted := evidence["submitted_at"]; submitted || row.Status == "unknown_needs_verify" {
		return ErrRenewalInProgress
	}
	return nil
}
