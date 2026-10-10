package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// SeatsChangeable reports whether a subscription's seats can change: an
// engine-owned NMI or Stripe subscription. Provider-owned and Solana
// subscriptions have none.
func SeatsChangeable(sub *models.Subscription) bool {
	return sub != nil && sub.CollectionPolicy == models.CollectionPolicyEngine && (sub.Rail == models.RailNMI || sub.Rail == models.RailStripe)
}

// LockSeatMembership locks and qualifies the engine membership a seat increase
// changes, under the caller's customer lock. Admission requires it open,
// unowned by any renewal and on the accepted card; completion only that it is
// the same obligation at the same seats.
func LockSeatMembership(ctx context.Context, d *db.DB, terms InitialMembershipTerms, rail models.Rail, admission bool) (*models.Subscription, error) {
	a := terms.Adds
	if d == nil || d.Pool() != nil || a == nil {
		return nil, errors.New("seat increase requires its transaction and accepted terms")
	}
	sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, terms.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub.CustomerID != terms.CustomerID || sub.PspID != terms.PSPID || sub.Rail != rail || !SeatsChangeable(sub) || sub.PriceID != terms.PriceID || sub.Quantity == nil || *sub.Quantity != a.FromQuantity || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(terms.PeriodEnd) {
		return nil, ErrUpgradeReplacedChanged
	}
	if !admission {
		if sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && (sub.Status != models.StatusCanceled || sub.CancelType == nil || *sub.CancelType != models.CancelTypeUser) {
			return nil, ErrUpgradeReplacedChanged
		}
		return sub, nil
	}
	if sub.Status != models.StatusActive || !terms.PeriodEnd.After(terms.AcceptedAt) || sub.DeletionScheduledAt != nil {
		return nil, ErrUpgradeRenewalDue
	}
	if charged, err := PaymentMethodOf(ctx, d.Gen(ctx), sub); err != nil {
		return nil, err
	} else if charged == nil || *charged != terms.PaymentMethodID {
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

// AddSeatsTx commits a paid seat increase in the caller's transaction: the
// membership bills the new seats from its next renewal, its access gives them
// from the accepted instant to the end it already had, and the charge is
// recorded with the seats it added. A pending change's seats are dropped. A
// staff change keeps its staff member and reason on the payment and queues
// the customer's notice.
func (s *SubscriptionLifecycleService) AddSeatsTx(ctx context.Context, txDB *db.DB, terms InitialMembershipTerms, rail models.Rail, transaction, custodian string, staff *StaffChange) error {
	sub, err := LockSeatMembership(ctx, txDB, terms, rail, false)
	if err != nil {
		return err
	}
	added := *terms.Quantity - terms.Adds.FromQuantity
	sub.Quantity = CloneQuantity(terms.Quantity)
	if err := NewSubscriptionRepo(txDB).UpdateAt(ctx, sub, s.now()); err != nil {
		return err
	}
	if err := keepPendingPrice(ctx, txDB, sub, terms.AcceptedAt); err != nil {
		return err
	}
	if terms.Amount > 0 {
		token := charge.TokenTypePSPToken
		if custodian != "" {
			token = payments.DefaultTokenType(string(rail), custodian)
		}
		kind := payments.AttemptInitial
		payment := &models.Payment{
			ID: terms.PaymentID, CustomerID: sub.CustomerID, PriceID: terms.PriceID, SubscriptionID: &sub.ID,
			Rail: rail, PspID: &terms.PSPID, TransactionID: transaction,
			Amount: terms.Amount, ListAmount: terms.RecurringAmount, Currency: terms.Currency,
			Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail,
			AttemptKind: &kind, TokenType: &token, Quantity: &added,
			Metadata:    withPaidPeriod(staff.Metadata(map[string]any{"added_seats": added}), terms.PeriodStart),
			PurchasedAt: terms.AcceptedAt, CreatedAt: s.now(),
		}
		if err := payments.NewPaymentService(txDB, s.Clock()).Create(ctx, payment); err != nil {
			return fmt.Errorf("record seat increase payment: %w", err)
		}
	}
	if err := s.resizeAccess(ctx, txDB, sub, terms.PeriodStart); err != nil || staff == nil {
		return err
	}
	_, err = QueueStaffChangeNotice(ctx, txDB, sub, StaffChangeNotice{
		Key: terms.PaymentID.String(), PriceID: terms.PriceID, ProductName: terms.ProductName, Quantity: terms.Quantity,
		Charged: terms.Amount, Currency: terms.Currency, TransactionID: transaction, NextAmount: terms.RecurringAmount, EffectiveAt: terms.AcceptedAt,
	})
	return err
}

// resizeAccess gives the subscription's current seats from at: its live
// window ends at at and a window of the new seats runs to the same end, and
// its renewal grace follows.
func (s *SubscriptionLifecycleService) resizeAccess(ctx context.Context, txDB *db.DB, sub *models.Subscription, at time.Time) error {
	window, err := txDB.Gen(ctx).GetLatestProductAccessBySource(ctx, gen.GetLatestProductAccessBySourceParams{
		MerchantID: sub.MerchantID, CustomerID: sub.CustomerID, ProductID: sub.ProductID,
		SourceType: string(models.AccessSourceSubscription), SourceID: sub.ID.String(),
	})
	live := err == nil && window.RevokedAt == nil && !window.StartsAt.After(at) && (window.EndsAt == nil || window.EndsAt.After(at))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	ent := s.newLifecycleEntitlementService(txDB)
	if err := ent.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, at, models.AccessRevokeSuperseded, models.AccessSourceSubscription, models.AccessSourceGrace); err != nil {
		return err
	}
	if !live {
		return nil
	}
	grant := subscriptionAccess(sub, at)
	grant.EndsAt, grant.Indefinite = window.EndsAt, window.EndsAt == nil
	if _, err := ent.PushAccess(ctx, grant); err != nil {
		return fmt.Errorf("resize access of %s: %w", sub.ID, err)
	}
	if sub.CurrentPeriodStartsAt != nil && sub.CurrentPeriodEndsAt != nil {
		return pushRenewalGrace(ctx, txDB, ent, sub, *sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt)
	}
	return nil
}

// RenewalChange is what a deferred change asks the next renewal of an engine
// subscription to bill, computed from the subscription and its pending change
// as read.
type RenewalChange struct {
	ExpectedPriceID  uuid.UUID
	ExpectedQuantity *int
	// ExpectedPending is the pending change the target was computed from.
	ExpectedPending *uuid.UUID
	PriceID         uuid.UUID
	// Quantity is the seats from the renewal on; nil for a price without them.
	Quantity *int
	// Staff schedule the change at the customer's request; they may also
	// cancel a pending price migration by asking for what the subscription
	// bills now.
	Staff *StaffChange
}

// ReplaceScheduledChange makes c the subscription's pending change, replacing
// a pending tier or seat change. A target that is what the subscription bills
// now only cancels the pending change and answers nil. Nothing is charged; a
// renewal that already froze its terms refuses, and so does a pending price
// migration unless staff cancel it.
func (r *SubscriptionRepo) ReplaceScheduledChange(ctx context.Context, id uuid.UUID, c RenewalChange, now time.Time) (*models.ScheduledChange, error) {
	var out *models.ScheduledChange
	err := r.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := r.db.NewWithPgxTx(tx)
		sub, err := NewSubscriptionRepo(d).GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if sub.PriceID != c.ExpectedPriceID || !SameQuantity(sub.Quantity, c.ExpectedQuantity) || sub.CollectionPolicy != models.CollectionPolicyEngine {
			return ErrUpgradeReplacedChanged
		}
		if c.Quantity != nil && !SeatsChangeable(sub) {
			return errors.New("only an NMI or Stripe engine subscription has seats")
		}
		pending, err := PendingChange(ctx, d, sub.ID)
		if err != nil {
			return err
		}
		if (pending == nil) != (c.ExpectedPending == nil) || (pending != nil && pending.ID != *c.ExpectedPending) {
			return ErrUpgradeReplacedChanged
		}
		clears := c.PriceID == sub.PriceID && SameQuantity(c.Quantity, sub.Quantity)
		if pending != nil {
			if pending.Source != billing.ScheduledChangeChange && !(clears && c.Staff != nil) {
				return ErrChangeAlreadyScheduled
			}
			if pending.PriceID == c.PriceID && SameQuantity(pending.Quantity, c.Quantity) {
				out = pending
				return nil
			}
			if _, err := CancelPendingChange(ctx, d, sub, now); err != nil {
				return err
			}
		}
		if clears {
			return RefuseOwnedRebillTerms(ctx, d, sub)
		}
		out, err = ScheduleChange(ctx, d, sub, NewScheduledChange{PriceID: c.PriceID, Quantity: CloneQuantity(c.Quantity), Source: billing.ScheduledChangeChange, Staff: c.Staff}, now)
		return err
	})
	return out, err
}

// keepPendingPrice settles the pending change when seats were added now: its
// seats no longer apply, its price (another tier's) still does.
func keepPendingPrice(ctx context.Context, d *db.DB, sub *models.Subscription, now time.Time) error {
	pending, err := PendingChange(ctx, d, sub.ID)
	if err != nil || pending == nil || pending.Source != billing.ScheduledChangeChange || pending.Quantity == nil {
		return err
	}
	if err := cancelChange(ctx, d, pending.ID, now); err != nil {
		return err
	}
	if pending.PriceID == sub.PriceID {
		return nil
	}
	_, err = insertChange(ctx, d, sub, NewScheduledChange{PriceID: pending.PriceID, Source: billing.ScheduledChangeChange}, now)
	return err
}

// StaffChangeNotice is the customer's receipt of a change staff made at their
// request: the plan and seats from EffectiveAt, and what it charged now.
type StaffChangeNotice struct {
	// Key names the change: its operation or scheduled change.
	Key           string
	PriceID       uuid.UUID
	ProductName   string
	Quantity      *int
	Charged       int64
	Currency      string
	TransactionID string
	NextAmount    int64
	EffectiveAt   time.Time
}

// QueueStaffChangeNotice queues the notice once per change in the caller's
// transaction; it is emailed after commit or by the notification sweep.
func QueueStaffChangeNotice(ctx context.Context, d *db.DB, sub *models.Subscription, n StaffChangeNotice) (*models.NotificationQueue, error) {
	next, at := n.NextAmount, n.EffectiveAt.UTC()
	data := billing.NotificationData{
		Source: "staff", Message: "Changed by support at your request.",
		SubscriptionID: billing.SubscriptionID(sub.ID), FromPriceID: billing.PriceID(sub.PriceID), ToPriceID: billing.PriceID(n.PriceID), ToProductName: n.ProductName,
		Quantity: CloneQuantity(n.Quantity), Currency: n.Currency, NewAmount: &next, EffectiveAt: &at, TransactionID: n.TransactionID,
	}
	if n.Charged > 0 {
		charged := n.Charged
		data.Amount = &charged
	}
	notice := &models.NotificationQueue{ID: uuid.NewSHA1(sub.ID, []byte("staff_change:"+n.Key)), CustomerID: sub.CustomerID, EventType: models.NotificationSubscriptionChanged, Data: data}
	if err := NewNotificationQueueRepo(d).CreateIfAbsent(ctx, notice); err != nil {
		return nil, fmt.Errorf("queue staff change notice for %s: %w", sub.ID, err)
	}
	return notice, nil
}
