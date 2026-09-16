package subscriptions

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// CompleteUpgradeTx commits the frozen upgrade's subscription, payment and
// access effects in the caller's transaction. The successor UUID is its replay
// identity; provider calls never occur here.
func (s *SubscriptionLifecycleService) CompleteUpgradeTx(ctx context.Context, txDB *db.DB, predecessor models.Subscription, next *models.Subscription, payment *models.Payment) error {
	repo := NewSubscriptionRepo(txDB)
	old, err := repo.GetByIDForUpdate(ctx, predecessor.ID)
	if err != nil {
		return err
	}
	if prior, err := repo.GetByID(ctx, next.ID); err == nil {
		if prior.CustomerID != next.CustomerID || prior.PriceID != next.PriceID || prior.PspID != next.PspID || prior.RailSubscriptionID != next.RailSubscriptionID {
			return fmt.Errorf("upgrade successor identity mismatch")
		}
		return nil
	} else if !db.IsNotFound(err) {
		return err
	}
	if old.CustomerID != next.CustomerID || old.PspID != next.PspID || old.PriceID != predecessor.PriceID || old.RailSubscriptionID != predecessor.RailSubscriptionID || old.Status != models.StatusActive {
		return fmt.Errorf("upgrade predecessor changed; provider receipts require reconciliation")
	}
	at := next.StartedAt
	reason := models.CancelType("upgrade")
	old.Status, old.CancelType = models.StatusCancelled, &reason
	old.CancelledAt, old.DeletionScheduledAt = &at, &at
	old.ClearRetrySchedule()
	if err := repo.UpdateAt(ctx, old, at); err != nil {
		return err
	}
	if err := repo.Create(ctx, next); err != nil {
		return err
	}
	ent := s.newLifecycleEntitlementService(txDB)
	if err := ent.RevokeSourcesForSubscriptionAsOf(ctx, old.CustomerID.String(), old.ID, at, models.EntitlementRevokeSuperseded, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
		return err
	}
	for name := range next.EntitlementsSpecSnapshot {
		if _, err := ent.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: next.CustomerID.String(), Entitlement: name, NotBefore: &at, EndAt: next.CurrentPeriodEndsAt, SourceType: models.EntitlementSourceSubscription, SourceID: next.ID}); err != nil {
			return err
		}
	}
	if payment != nil {
		if err := payments.NewPaymentService(txDB, s.Clock()).Create(ctx, payment); err != nil {
			return err
		}
	}
	return s.grantSubscriptionCreditsTx(ctx, txDB, next, models.CreditGrantCadenceOnce, "subscription_initial")
}
