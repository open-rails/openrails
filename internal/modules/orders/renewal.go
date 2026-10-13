package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A renewal of an engine-owned subscription is an order (origin
// subscription): one per subscription and period, due when the period
// starts. Each collection operation the due pass or the customer starts is
// one of its attempts (the attempt's id is the operation's); the operation's
// completion settles the order in its own transaction. The subscription's
// lifecycle (period, dunning) stays the subscription's; the order is what
// the customer owes and pays.

// Renewal is one period of a subscription, at its frozen terms.
type Renewal struct {
	SubscriptionID uuid.UUID
	CustomerID     uuid.UUID
	PriceID        uuid.UUID
	ProductID      uuid.UUID
	Description    string
	// Quantity is the seats of a per-seat price; Amount is the period's
	// total.
	Quantity             *int
	Amount               int64
	Currency             string
	PeriodStart          time.Time
	PeriodEnd            time.Time
	BillingIntervalHours *int
	AccessDurationHours  *int
}

// OpenRenewal is the order of r's period, created the first time.
func OpenRenewal(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, r Renewal, now time.Time) (gen.BillingOrder, error) {
	if r.SubscriptionID == uuid.Nil || r.CustomerID == uuid.Nil || r.PriceID == uuid.Nil || !r.PeriodEnd.After(r.PeriodStart) || r.Amount < 0 {
		return gen.BillingOrder{}, errors.New("a renewal order needs its subscription, customer, price and period")
	}
	if err := db.EnsureCustomerRowQ(ctx, q, merchantID, r.CustomerID); err != nil {
		return gen.BillingOrder{}, err
	}
	id := uuidutil.NewV7()
	start := r.PeriodStart.UTC()
	n, err := q.CreateRenewalOrder(ctx, gen.CreateRenewalOrderParams{MerchantID: merchantID, ID: id, CustomerID: r.CustomerID, Currency: r.Currency, Total: r.Amount,
		SubscriptionID: r.SubscriptionID, PeriodStartsAt: start, PeriodEndsAt: r.PeriodEnd.UTC(), DueAt: start, Now: now})
	if err != nil {
		return gen.BillingOrder{}, err
	}
	if n == 1 {
		quantity, err := int32Ptr(r.Quantity)
		if err != nil {
			return gen.BillingOrder{}, err
		}
		interval, err := int32Ptr(r.BillingIntervalHours)
		if err != nil {
			return gen.BillingOrder{}, err
		}
		access, err := int32Ptr(r.AccessDurationHours)
		if err != nil {
			return gen.BillingOrder{}, err
		}
		unit := r.Amount
		if r.Quantity != nil && *r.Quantity > 1 {
			unit = r.Amount / int64(*r.Quantity)
		}
		// The subscription holds the ownership: the line names it, and no
		// order claims it.
		product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: merchantID, ID: r.ProductID})
		if err != nil {
			return gen.BillingOrder{}, err
		}
		claim := claimKeyOf(product.ID, product.TierGroup)
		line := uuidutil.NewV7()
		if err := q.CreateOrderLine(ctx, gen.CreateOrderLineParams{MerchantID: merchantID, ID: line, OrderID: id, CustomerID: r.CustomerID, Position: 0,
			PriceID: r.PriceID, ProductID: r.ProductID, Description: r.Description, Quantity: quantity, UnitAmount: unit, Amount: r.Amount,
			Ownership: string(catalog.OwnershipUnique), ClaimKey: &claim, BillingIntervalHours: interval, AccessDurationHours: access, Now: now}); err != nil {
			return gen.BillingOrder{}, err
		}
		if err := q.SetOrderLineProduced(ctx, gen.SetOrderLineProducedParams{MerchantID: merchantID, ID: line, SubscriptionID: &r.SubscriptionID}); err != nil {
			return gen.BillingOrder{}, err
		}
	}
	return q.GetRenewalOrder(ctx, gen.GetRenewalOrderParams{MerchantID: merchantID, SubscriptionID: r.SubscriptionID, PeriodStartsAt: start})
}

// StartRenewalAttempt puts one collection operation on the renewal order:
// its payment is with the provider until the operation completes.
func StartRenewalAttempt(ctx context.Context, q *gen.Queries, order gen.BillingOrder, operation, psp uuid.UUID, rail string, method *uuid.UUID, now time.Time) error {
	if order.Origin != string(billing.OrderOriginSubscription) || order.PeriodEndsAt == nil {
		return errors.New("a renewal attempt pays a renewal order")
	}
	if order.Status != string(billing.OrderOpen) {
		return fmt.Errorf("renewal order %s is %s", order.ID, order.Status)
	}
	state, err := json.Marshal(map[string]any{"operation_id": operation.String()})
	if err != nil {
		return err
	}
	if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{ID: operation, MerchantID: order.MerchantID, CustomerID: order.CustomerID, OrderID: order.ID,
		Rail: rail, Status: "processing", Amount: order.Total, Currency: order.Currency, ExpiresAt: *order.PeriodEndsAt, RailState: state, PspID: psp, Now: now}); err != nil {
		return err
	}
	if n, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: order.MerchantID, ID: order.ID, AttemptID: operation, PaymentMethodID: method, PspID: psp, Now: now}); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("renewal order %s took no attempt", order.ID))
	}
	_, err = q.SetOrderPending(ctx, gen.SetOrderPendingParams{MerchantID: order.MerchantID, ID: order.ID, AttemptID: operation, PaymentStatus: string(billing.OrderPaymentProcessing), Now: now})
	return err
}

// RenewalPaid completes the renewal order its collection paid, in the
// collection's completion transaction: numbered, linked to the payment the
// renewal recorded, with order.completed. A renewal whose subscription ended
// meanwhile is void instead, the payment (refunded) on it; payment is nil
// when a reversal recorded none.
func RenewalPaid(ctx context.Context, q *gen.Queries, merchantID, orderID, attemptID uuid.UUID, payment *uuid.UUID, transactionID string, now time.Time) error {
	order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: orderID})
	if err != nil {
		return err
	}
	if payment != nil {
		n, err := q.LinkAcceptedRenewalPaymentOrder(ctx, gen.LinkAcceptedRenewalPaymentOrderParams{MerchantID: merchantID, OrderID: orderID, PaymentID: *payment})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("renewal payment does not belong to this order")
		}
	}
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: merchantID, ID: attemptID, Status: "succeeded", PaymentID: payment, TransactionID: &transactionID, Now: now}); err != nil {
		return err
	}
	if order.Status != string(billing.OrderOpen) && order.Status != string(billing.OrderProcessing) {
		return nil
	}
	ended, err := renewalEnded(ctx, q, order)
	if err != nil {
		return err
	}
	if ended {
		return voidRenewal(ctx, q, &order, payment, now)
	}
	if payment == nil {
		return fmt.Errorf("renewal order %s was paid without a payment", orderID)
	}
	number, err := q.NextOrderNumber(ctx)
	if err != nil {
		return err
	}
	if n, err := q.SetOrderComplete(ctx, gen.SetOrderCompleteParams{MerchantID: merchantID, ID: orderID, Number: number, PaymentID: payment, Now: now}); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("renewal order %s did not complete", orderID))
	}
	order.Status, order.PaymentStatus = string(billing.OrderComplete), string(billing.OrderPaymentSucceeded)
	return orderEvent(ctx, q, &order, "order.completed", now, map[string]any{"number": number, "payment_id": payment.String()})
}

// RenewalUnpaid ends a collection attempt that moved no money: the renewal
// order is open again, with why, or void once its subscription ended.
func RenewalUnpaid(ctx context.Context, q *gen.Queries, merchantID, orderID, attemptID uuid.UUID, failure billing.PaymentFailure, now time.Time) error {
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: merchantID, ID: attemptID, Status: "failed", Now: now}); err != nil {
		return err
	}
	order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: orderID})
	if err != nil {
		return err
	}
	if order.AttemptID == nil || *order.AttemptID != attemptID || (order.Status != string(billing.OrderOpen) && order.Status != string(billing.OrderProcessing)) {
		return nil
	}
	ended, err := renewalEnded(ctx, q, order)
	if err != nil {
		return err
	}
	if ended {
		return voidRenewal(ctx, q, &order, nil, now)
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	if _, err := q.SetOrderDeclined(ctx, gen.SetOrderDeclinedParams{MerchantID: merchantID, ID: orderID, AttemptID: attemptID, LastPaymentError: raw, Now: now}); err != nil {
		return err
	}
	order.Status, order.PaymentStatus = string(billing.OrderOpen), string(billing.OrderPaymentRequiresPaymentMethod)
	return orderEvent(ctx, q, &order, "order.payment_failed", now, map[string]any{"reason": failure.Reason})
}

// renewalEnded reports a renewal order whose subscription ended.
func renewalEnded(ctx context.Context, q *gen.Queries, order gen.BillingOrder) (bool, error) {
	sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: order.MerchantID, ID: *order.SubscriptionID})
	if err != nil {
		return false, err
	}
	return sub.Status == "canceled" || sub.DeletedAt != nil, nil
}

// voidRenewal voids a live renewal order, with order.voided.
func voidRenewal(ctx context.Context, q *gen.Queries, order *gen.BillingOrder, payment *uuid.UUID, now time.Time) error {
	n, err := q.VoidRenewalOrder(ctx, gen.VoidRenewalOrderParams{MerchantID: order.MerchantID, ID: order.ID, PaymentID: payment, Now: now})
	if err != nil || n == 0 {
		return err
	}
	order.Status, order.PaymentStatus = string(billing.OrderVoid), string(billing.OrderPaymentRequiresPaymentMethod)
	extra := map[string]any{}
	if payment != nil {
		order.PaymentStatus = string(billing.OrderPaymentSucceeded)
		extra["payment_id"] = payment.String()
	}
	return orderEvent(ctx, q, order, "order.voided", now, extra)
}

// RenewalAwaitsCustomer marks the renewal order's payment as waiting for the
// customer's authentication; it reports whether it was not already.
func RenewalAwaitsCustomer(ctx context.Context, q *gen.Queries, merchantID, orderID, attemptID uuid.UUID, now time.Time) (bool, error) {
	order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: orderID})
	if err != nil {
		return false, err
	}
	if order.AttemptID == nil || *order.AttemptID != attemptID || order.PaymentStatus == string(billing.OrderPaymentRequiresAction) {
		return false, nil
	}
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: merchantID, ID: attemptID, Status: "requires_action", Now: now}); err != nil {
		return false, err
	}
	n, err := q.SetOrderPending(ctx, gen.SetOrderPendingParams{MerchantID: merchantID, ID: orderID, AttemptID: attemptID, PaymentStatus: string(billing.OrderPaymentRequiresAction), Now: now})
	if err != nil || n == 0 {
		return false, err
	}
	order.Status, order.PaymentStatus = string(billing.OrderOpen), string(billing.OrderPaymentRequiresAction)
	return true, orderEvent(ctx, q, &order, "order.requires_action", now, nil)
}
