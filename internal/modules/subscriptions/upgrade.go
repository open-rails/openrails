package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

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
		return payments.NewPaymentService(txDB, s.Clock()).Create(ctx, payment)
	}
	return nil
}

// ErrUpgradeRenewalDue refuses an engine upgrade while the membership it would
// replace has an ended period or an unresolved renewal: the renewal settles
// first, so one paid period is never billed by two operations.
var ErrUpgradeRenewalDue = errors.New("engine upgrade waits for the current renewal to settle")

// ErrRenewalHeldByUpgrade defers a due renewal while an unresolved engine
// upgrade owns the membership; the next due pass admits it if the upgrade is
// refused.
var ErrRenewalHeldByUpgrade = errors.New("renewal waits for an unresolved tier upgrade")

// ErrUpgradeReplacedChanged refuses an engine upgrade whose replaced
// membership no longer matches the terms the customer accepted.
var ErrUpgradeReplacedChanged = errors.New("membership changed since the upgrade was quoted")

// LockReplacedMembership locks and qualifies the engine membership an upgrade
// replaces, under the caller's customer lock. Admission requires it open and
// unowned by any renewal; completion (admission false) only that it is the
// same obligation the customer accepted to replace.
func LockReplacedMembership(ctx context.Context, d *db.DB, terms InitialMembershipTerms, rail models.Rail, admission bool) (*models.Subscription, error) {
	r := terms.Replaces
	if d == nil || d.Pool() != nil || r == nil {
		return nil, errors.New("replaced membership requires its transaction and accepted terms")
	}
	sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, r.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.CustomerID != terms.CustomerID || sub.PspID != terms.PSPID || sub.Rail != rail || sub.CollectionPolicy != models.CollectionPolicyEngine || sub.PriceID != r.PriceID || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(r.PeriodEnd) {
		return nil, ErrUpgradeReplacedChanged
	}
	if !admission {
		if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && (sub.Status != models.StatusCancelled || sub.CancelType == nil || *sub.CancelType != models.CancelTypeUser) {
			return nil, ErrUpgradeReplacedChanged
		}
		return sub, nil
	}
	if sub.Status != models.StatusActive || !r.PeriodEnd.After(terms.AcceptedAt) || sub.DeletionScheduledAt != nil {
		return nil, ErrUpgradeRenewalDue
	}
	if sub.PaymentMethodID == nil || *sub.PaymentMethodID != terms.PaymentMethodID {
		return nil, ErrUpgradeReplacedChanged
	}
	if err := RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
		if errors.Is(err, ErrRebillTermsCommitted) {
			return nil, ErrUpgradeRenewalDue
		}
		return nil, err
	}
	return sub, nil
}

// SupersedeForUpgradeTx ends the replaced engine membership at the instant
// the paid upgrade's period starts, before the successor is created in the
// same transaction (one live membership per tier group). Its unused value was
// credited in the upgrade price; its access ends as superseded.
func (s *SubscriptionLifecycleService) SupersedeForUpgradeTx(ctx context.Context, txDB *db.DB, terms InitialMembershipTerms, rail models.Rail) error {
	old, err := LockReplacedMembership(ctx, txDB, terms, rail, false)
	if err != nil {
		return err
	}
	at := terms.PeriodStart
	reason := models.CancelTypeUpgrade
	old.Status, old.CancelType, old.CancelledAt, old.EndedAt, old.ScheduledPriceID = models.StatusCancelled, &reason, &at, &at, nil
	old.CurrentPeriodEndsAt = &at
	if old.CurrentPeriodStartsAt != nil && !old.CurrentPeriodStartsAt.Before(at) {
		start := at.Add(-time.Microsecond)
		old.CurrentPeriodStartsAt = &start
	}
	old.ClearRetrySchedule()
	if err := NewSubscriptionRepo(txDB).UpdateAt(ctx, old, s.now()); err != nil {
		return err
	}
	return s.newLifecycleEntitlementService(txDB).RevokeSourcesForSubscriptionAsOf(ctx, old.CustomerID.String(), old.ID, at, models.EntitlementRevokeSuperseded, models.EntitlementSourceSubscription, models.EntitlementSourceGrace)
}
