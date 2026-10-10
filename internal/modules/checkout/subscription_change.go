package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	log "github.com/sirupsen/logrus"
)

// A subscription change moves a subscription to another price of its tier
// group, to another number of seats, or both, the way Stripe's subscription
// update does:
//
//   - more seats on the same price apply now and charge the added seats for
//     the rest of the period (unit × added × remaining fraction);
//   - fewer seats apply at the next renewal, with no credit;
//   - a tier upgrade applies now and prorates the new price × seats against
//     the old price × seats; a tier downgrade applies at the next renewal with
//     its seats;
//   - a change to what the subscription bills now cancels its pending change;
//   - staff change at the customer's request the same way, charging now
//     merchant-initiated under the card's recurring agreement, with their
//     reason kept and the customer told; they never charge a subscription
//     its provider bills.
//
// Seats exist only for a price sold per seat (its catalog quantity bounds),
// on engine-owned NMI and Stripe subscriptions.

var (
	errSeatsUnsupported    = &TierChangeError{Code: billing.CodeSubscriptionChangeUnsupportedOnRail, Message: "only a subscription billed by OpenRails on a card rail has seats"}
	errNothingToChange     = &TierChangeError{Code: billing.CodeResourceConflict, Message: "the subscription already has this price and these seats"}
	errQuantityNotAllowed  = &TierChangeError{Code: billing.CodeQuantityNotAllowed, Message: "the price is not sold per seat; send no quantity"}
	errStaffProviderCharge = &TierChangeError{Code: billing.CodeCustomerActionRequired, Message: "staff cannot charge a subscription its provider bills; the customer makes this upgrade"}
	errProviderHeldChange  = &TierChangeError{Code: billing.CodeSubscriptionChangeProviderConflict, Message: "the provider already bills the scheduled change; it cannot be changed back here"}
)

// changeTarget is a resolved change: the subscription as read, its current
// price and product, and the price, product and seats it moves to.
type changeTarget struct {
	sub *models.Subscription
	// charged is the card the subscription charges: its own, or its default.
	charged        *uuid.UUID
	currentPrice   *models.Price
	currentProduct *models.Product
	price          *models.Price
	product        *models.Product
	// quantity is the seats it moves to; nil for a price not sold per seat.
	quantity *int
	// tier: the price moves to another product of the tier group; upgrade: to
	// one ranked no lower.
	tier, upgrade bool
	// priceGiven: the request named a price.
	priceGiven bool
	// requested is the seats the request named.
	requested *int
	// pending is the subscription's scheduled change, as read.
	pending *models.ScheduledChange
	// staff made the change at the customer's request.
	staff *subscriptions.StaffChange
}

// staffChange is the staff member and reason a staff change keeps; nil for
// the customer's own.
func staffChange(req *SubscriptionChangeRequest) *subscriptions.StaffChange {
	if !req.Staff {
		return nil
	}
	return &subscriptions.StaffChange{Invoker: req.Invoker, Reason: req.Reason}
}

// clears reports whether the change asks for what the subscription bills
// now: it only cancels the pending change.
func (c *changeTarget) clears() bool {
	return !c.tier && c.price.ID == c.sub.PriceID && subscriptions.SameQuantity(c.quantity, c.sub.Quantity)
}

// deferred reports whether a change waits for the next renewal: a tier
// downgrade or fewer seats.
func (c *changeTarget) deferred() bool {
	if c.tier {
		return !c.upgrade
	}
	return c.quantity == nil || c.sub.Quantity == nil || *c.quantity < *c.sub.Quantity
}

// ChangeSubscription changes a subscription's price, seats or both; every
// instant in the answer is UTC.
func (s *CheckoutService) ChangeSubscription(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity) (*TierChangeResponse, error) {
	resp, err := s.changeSubscription(ctx, req, user)
	if resp != nil {
		resp.DelayedStart, resp.NextChargeDate = utcPtr(resp.DelayedStart), utcPtr(resp.NextChargeDate)
	}
	return resp, err
}

func (s *CheckoutService) changeSubscription(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity) (*TierChangeResponse, error) {
	if response, found, err := s.ReplayTierChange(ctx, req, user); found || err != nil {
		return response, err
	}
	c, err := s.resolveChange(ctx, req, user)
	if err != nil {
		return nil, err
	}
	// One unresolved change owns the subscription on every rail: a request
	// under another key is pointed at it.
	if err := s.refuseTierChangeInFlight(ctx, c.sub); err != nil {
		return nil, err
	}
	resp, err := s.routeChange(ctx, req, user, c)
	// A staff change charged now is told from its completion; one scheduled
	// or canceled here is told now.
	if err == nil && c.staff != nil && resp.Status == "succeeded" && (resp.Effective != "now" || c.clears()) {
		s.notifyStaffChange(ctx, c, resp)
	}
	return resp, err
}

// notifyStaffChange tells the customer of a change staff made at their
// request that charged nothing. A failure is logged: the change stands.
func (s *CheckoutService) notifyStaffChange(ctx context.Context, c *changeTarget, resp *TierChangeResponse) {
	product := c.product.DisplayName
	if resp.PriceID != billing.PriceID(c.price.ID) {
		if price, err := s.PriceService.GetByID(ctx, resp.PriceID.UUID()); err == nil {
			if p, err := s.ProductService.GetByID(ctx, price.ProductID); err == nil {
				product = p.DisplayName
			}
		}
	}
	at := s.now()
	if resp.Effective != "now" && c.sub.CurrentPeriodEndsAt != nil {
		at = *c.sub.CurrentPeriodEndsAt
	}
	key := fmt.Sprintf("%s:%s:%d:%s", resp.Effective, resp.PriceID, subscriptions.SeatCount(resp.Quantity), at.UTC().Format(time.RFC3339Nano))
	if c.clears() && c.pending != nil {
		key = "canceled:" + c.pending.ID.String()
	}
	notice, err := subscriptions.QueueStaffChangeNotice(ctx, s.SubscriptionService.Database(), c.sub, subscriptions.StaffChangeNotice{
		Key: key, PriceID: resp.PriceID.UUID(), ProductName: product, Quantity: resp.Quantity,
		Currency: resp.Currency, NextAmount: resp.NextChargeAmount, EffectiveAt: at,
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", c.sub.ID).Error("queue staff change notice")
		return
	}
	if s.Lifecycle != nil {
		s.Lifecycle.DispatchNotifications(ctx, []*models.NotificationQueue{notice})
	}
}

func (s *CheckoutService) routeChange(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity, c *changeTarget) (*TierChangeResponse, error) {
	sub := c.sub
	if sub.CollectionPolicy == models.CollectionPolicyEngine && sub.Rail != models.RailSolana {
		switch {
		case c.clears():
			return s.cancelEngineChange(ctx, c)
		case c.deferred():
			return s.scheduleEngineChange(ctx, c)
		case c.tier:
			return s.processEngineUpgrade(ctx, req, user, c)
		default:
			return s.addEngineSeats(ctx, req, user, c)
		}
	}
	if c.clears() {
		return nil, errProviderHeldChange
	}
	if !c.tier {
		return nil, errNothingToChange
	}
	// Staff cannot charge a provider-owned subscription: the provider, not
	// OpenRails, would charge without the card's agreement checked.
	if req.Staff && c.upgrade {
		return nil, errStaffProviderCharge
	}
	action := "upgrade"
	if !c.upgrade {
		action = "downgrade"
	}
	switch {
	case sub.Rail == models.RailStripe:
		return s.processTierChangeStripe(ctx, req, user, c.price, c.product, sub, c.currentProduct, action)
	case rails.IsNMI(sub.Rail):
		return s.processProviderNMITierChange(ctx, req, user, c.price, c.product, sub, action)
	case sub.Rail == models.RailCCBill && !req.Staff:
		return s.processTierChangeCCBill(ctx, req, user, c.price, c.product, sub, c.currentProduct, action)
	case sub.Rail == models.RailSolana && !req.Staff:
		return s.processTierChangeSolana(ctx, req, user, c.price, c.product, sub, c.currentProduct, action)
	case sub.Rail == models.RailCCBill || sub.Rail == models.RailSolana:
		return nil, &TierChangeError{Code: billing.CodeCustomerActionRequired, Message: "this subscription changes only through the customer's own step on its rail"}
	default:
		return nil, &TierChangeError{Code: billing.CodeSubscriptionChangeUnsupportedOnRail, Message: fmt.Sprintf("unsupported rail: %s", sub.Rail)}
	}
}

// PreviewSubscriptionChange quotes what ChangeSubscription would charge now
// and at the next renewal, without changing anything. A Stripe provider
// subscription's charge now is an estimate: Stripe finalizes the proration.
func (s *CheckoutService) PreviewSubscriptionChange(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity) (*TierChangePreviewResponse, error) {
	resp, err := s.previewSubscriptionChange(ctx, req, user)
	if resp != nil {
		resp.NextChargeDate = utcPtr(resp.NextChargeDate)
	}
	return resp, err
}

func (s *CheckoutService) previewSubscriptionChange(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity) (*TierChangePreviewResponse, error) {
	c, err := s.resolveChange(ctx, req, user)
	if err != nil {
		return nil, err
	}
	sub := c.sub
	resp := &TierChangePreviewResponse{PriceID: billing.PriceID(c.price.ID), Quantity: subscriptions.CloneQuantity(c.quantity), Rail: string(sub.Rail), Currency: c.price.Currency}
	if sub.CollectionPolicy == models.CollectionPolicyEngine && sub.Rail != models.RailSolana {
		switch {
		case c.clears():
			return previewEngineCancel(resp, c)
		case c.deferred():
			return s.previewEngineSchedule(ctx, resp, c)
		case c.tier:
			return s.previewEngineUpgrade(resp, c)
		default:
			return s.previewEngineSeats(resp, c)
		}
	}
	if c.clears() {
		return nil, errProviderHeldChange
	}
	if !c.tier {
		return nil, errNothingToChange
	}
	if req.Staff && c.upgrade {
		return nil, errStaffProviderCharge
	}
	deferred := !c.upgrade
	if rails.IsNMI(sub.Rail) {
		return s.previewProviderNMITierChange(ctx, resp, sub, c.currentPrice, c.price, c.product, deferred)
	}
	if req.Staff && (sub.Rail == models.RailCCBill || sub.Rail == models.RailSolana) {
		return nil, &TierChangeError{Code: billing.CodeCustomerActionRequired, Message: "this subscription changes only through the customer's own step on its rail"}
	}
	resp.NextChargeAmount = c.price.Amount
	if deferred {
		if err := s.validateTierChangePreviewTarget(ctx, sub, c.currentPrice, c.price, user, "downgrade"); err != nil {
			return nil, err
		}
		if c.pending != nil {
			return nil, ErrTierChangePending
		}
		resp.Effective, resp.NextChargeDate = "period_end", sub.CurrentPeriodEndsAt
		if sub.CurrentPeriodEndsAt != nil {
			resp.Message = fmt.Sprintf("No charge now. The plan changes to %s on %s, then renews at %s.",
				c.product.DisplayName, sub.CurrentPeriodEndsAt.UTC().Format("January 2, 2006"), formatMinorAmount(c.price.Amount, c.price.Currency))
		}
		return resp, nil
	}
	if err := s.validateTierChangePreviewTarget(ctx, sub, c.currentPrice, c.price, user, "upgrade"); err != nil {
		return nil, err
	}
	var quote ModelBUpgradeQuote
	if sub.Rail == models.RailSolana {
		quote, err = QuoteSolanaUpgrade(ctx, s.SubscriptionService.Database(), sub, c.currentPrice, c.price, s.now())
	} else {
		quote, err = QuoteModelBUpgrade(providerUpgradeOf(sub, c.currentPrice, c.price), s.now())
	}
	if err != nil {
		return nil, err
	}
	next := quote.PeriodEnd
	resp.Effective, resp.AmountDueNow, resp.NextChargeDate, resp.IsEstimate = "now", quote.ChargeNow, &next, sub.Rail == models.RailStripe
	resp.Message = fmt.Sprintf("You'll be charged %s now and %s on %s.", formatMinorAmount(quote.ChargeNow, c.price.Currency), formatMinorAmount(c.price.Amount, c.price.Currency), next.UTC().Format("January 2, 2006"))
	return resp, nil
}

// resolveChange reads the subscription the customer owns and what the request
// moves it to, refusing what no rail can do.
func (s *CheckoutService) resolveChange(ctx context.Context, req *SubscriptionChangeRequest, user *UserIdentity) (*changeTarget, error) {
	if strings.TrimSpace(req.PriceID) == "" && req.Quantity == nil {
		return nil, &TierChangeError{Code: billing.CodeInvalidParam, Message: "a change names a price_id, a quantity or both"}
	}
	var sub *models.Subscription
	var err error
	if req.SubscriptionID != uuid.Nil {
		sub, err = s.SubscriptionService.GetByID(ctx, req.SubscriptionID)
		// Ownership compares parsed customer ids, not their spelling.
		if payer := identity.CustomerIDFromString(user.ID); err != nil || payer.IsZero() || sub.CustomerID != payer.UUID() {
			return nil, &TierChangeError{Code: codeSubscriptionNotFound, Message: "subscription not found"}
		}
	} else if sub, err = s.SubscriptionService.GetActiveSubscription(ctx, user.ID); err != nil {
		return nil, ErrTierChangeNoSubscription
	}
	if err := validateTierChangeSubscriptionStatus(sub); err != nil {
		return nil, err
	}
	c := &changeTarget{sub: sub, requested: req.Quantity, staff: staffChange(req)}
	if c.charged, err = subscriptions.PaymentMethodOf(ctx, s.SubscriptionService.Database().Gen(ctx), sub); err != nil {
		return nil, err
	}
	if c.pending, err = subscriptions.PendingChange(ctx, s.SubscriptionService.Database(), sub.ID); err != nil {
		return nil, err
	}
	if c.currentPrice, err = s.PriceService.GetByID(ctx, sub.PriceID); err != nil {
		return nil, &TierChangeError{Code: billing.CodeInternalError, Message: "current price not found"}
	}
	sub.Price = c.currentPrice
	if c.currentProduct, err = s.ProductService.GetByID(ctx, c.currentPrice.ProductID); err != nil {
		return nil, &TierChangeError{Code: billing.CodeInternalError, Message: "current product not found"}
	}
	c.price, c.product = c.currentPrice, c.currentProduct
	if ref := strings.TrimSpace(req.PriceID); ref != "" {
		c.priceGiven = true
		if c.price, err = catalog.ResolveReference(ctx, s.PriceService, "", ref); err != nil {
			return nil, &TierChangeError{Code: codePriceNotFound, Message: "price not found"}
		}
		if c.product, err = s.ProductService.GetByID(ctx, c.price.ProductID); err != nil {
			return nil, &TierChangeError{Code: codeProductNotFound, Message: "product not found"}
		}
		c.tier = c.product.ID != c.currentProduct.ID
		if c.tier && (!c.price.IsPurchasable() || !c.product.IsPurchasable()) {
			return nil, &TierChangeError{Code: billing.CodeSubscriptionChangeTargetInactive, Message: "price is not available"}
		}
		if !c.tier && c.price.ID != c.currentPrice.ID {
			return nil, ErrTierChangeSameProduct
		}
	}
	if c.tier {
		if !sameTierGroup(c.currentProduct, c.product) {
			return nil, ErrTierChangeDifferentGroup
		}
		// A tier group may mix currencies; a subscription never moves across
		// one.
		if err := RequireSameCurrency(PriceAmountOf(c.currentPrice), PriceAmountOf(c.price)); err != nil {
			return nil, err
		}
		if sub.Rail == models.RailSolana {
			c.upgrade, err = SolanaTierChange(sub, c.currentProduct, c.product, c.currentPrice, c.price)
			if err != nil {
				return nil, err
			}
		} else {
			c.upgrade = c.product.TierRank >= c.currentProduct.TierRank
		}
	}
	if c.quantity, err = changeQuantity(req, sub, c.price); err != nil {
		return nil, err
	}
	if c.quantity != nil && !subscriptions.SeatsChangeable(sub) {
		return nil, errSeatsUnsupported
	}
	// What the subscription bills now is a change only while another is
	// pending; a customer cannot cancel a merchant's price migration.
	if c.clears() && (c.pending == nil || (c.pending.Source != billing.ScheduledChangeChange && !req.Staff)) {
		return nil, errNothingToChange
	}
	return c, nil
}

// changeQuantity is the seats a change moves to: those requested, within the
// target price's bounds; else the current ones (a price newly per seat starts
// at its minimum); nil for a price not sold per seat, which refuses any.
func changeQuantity(req *SubscriptionChangeRequest, sub *models.Subscription, target *models.Price) (*int, error) {
	bounds := target.Quantity
	if bounds == nil {
		if req.Quantity != nil {
			return nil, errQuantityNotAllowed
		}
		return nil, nil
	}
	q := bounds.Min
	switch {
	case req.Quantity != nil:
		q = *req.Quantity
	case sub.Quantity != nil:
		q = *sub.Quantity
	}
	if !bounds.Allows(q) {
		return nil, &TierChangeError{Code: billing.CodeInvalidParam, Message: fmt.Sprintf("quantity must be between %d and %d for this price", bounds.Min, bounds.Max)}
	}
	return &q, nil
}

// seatPrice is price × quantity, the price for one without seats.
func seatPrice(p *models.Price, quantity *int) (int64, error) {
	if p == nil {
		return 0, errors.New("price is required")
	}
	return subscriptions.SeatAmount(p.Amount, quantity)
}
