package invoices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/notificationinbox"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A renewal of an engine-owned subscription is an invoice (origin
// subscription): one per subscription and period, due when the period
// starts. Each collection operation the due pass or the customer starts is
// one of its attempts (the attempt's id is the operation's); the operation's
// completion settles the invoice in its own transaction. The subscription's
// lifecycle (period, dunning) stays the subscription's; the invoice is what
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
	Quantity              *int
	Amount                int64
	Currency              string
	PeriodStart           time.Time
	PeriodEnd             time.Time
	BillingIntervalHours  *int
	BillingIntervalMonths *int
	AccessDurationHours   *int
	CreditGrant           *models.CreditGrantSnapshot
}

// OpenRenewal is the invoice of r's period, created the first time.
func OpenRenewal(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, r Renewal, now time.Time) (gen.BillingInvoice, error) {
	if r.SubscriptionID == uuid.Nil || r.CustomerID == uuid.Nil || r.PriceID == uuid.Nil || !r.PeriodEnd.After(r.PeriodStart) || r.Amount < 0 {
		return gen.BillingInvoice{}, errors.New("a renewal invoice needs its subscription, customer, price and period")
	}
	if err := db.EnsureCustomerRowQ(ctx, q, merchantID, r.CustomerID); err != nil {
		return gen.BillingInvoice{}, err
	}
	id := uuidutil.NewV7()
	start := r.PeriodStart.UTC()
	n, err := q.CreateRenewalInvoice(ctx, gen.CreateRenewalInvoiceParams{MerchantID: merchantID, ID: id, CustomerID: r.CustomerID, Currency: r.Currency, Total: r.Amount,
		SubscriptionID: r.SubscriptionID, PeriodStartsAt: start, PeriodEndsAt: r.PeriodEnd.UTC(), DueAt: start, Now: now})
	if err != nil {
		return gen.BillingInvoice{}, err
	}
	if n == 1 {
		if err := r.CreditGrant.Validate(); err != nil {
			return gen.BillingInvoice{}, err
		}
		credit, err := json.Marshal(r.CreditGrant)
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		quantity, err := int32Ptr(r.Quantity)
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		interval, err := int32Ptr(r.BillingIntervalHours)
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		months, err := int32Ptr(r.BillingIntervalMonths)
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		access, err := int32Ptr(r.AccessDurationHours)
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		unit := r.Amount
		if r.Quantity != nil && *r.Quantity > 1 {
			unit = r.Amount / int64(*r.Quantity)
		}
		// The subscription holds the ownership: the line names it, and no
		// invoice claims it.
		product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: merchantID, ID: r.ProductID})
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		claim := claimKeyOf(product.ID, product.TierGroup)
		line := uuidutil.NewV7()
		if err := q.CreateInvoiceLine(ctx, gen.CreateInvoiceLineParams{MerchantID: merchantID, ID: line, InvoiceID: id, CustomerID: r.CustomerID, Position: 0,
			PriceID: r.PriceID, ProductID: r.ProductID, Description: r.Description, Quantity: quantity, UnitAmount: unit, Amount: r.Amount,
			Ownership: string(catalog.OwnershipUnique), ClaimKey: &claim, BillingIntervalHours: interval, BillingIntervalMonths: months, AccessDurationHours: access, CreditGrant: credit, Now: now}); err != nil {
			return gen.BillingInvoice{}, err
		}
		if err := q.SetInvoiceLineProduced(ctx, gen.SetInvoiceLineProducedParams{MerchantID: merchantID, ID: line, SubscriptionID: &r.SubscriptionID}); err != nil {
			return gen.BillingInvoice{}, err
		}
	}
	return q.GetRenewalInvoice(ctx, gen.GetRenewalInvoiceParams{MerchantID: merchantID, SubscriptionID: r.SubscriptionID, PeriodStartsAt: start})
}

// StartRenewalAttempt puts one collection operation on the renewal invoice:
// its payment is with the provider until the operation completes.
func StartRenewalAttempt(ctx context.Context, q *gen.Queries, invoice gen.BillingInvoice, operation, psp uuid.UUID, rail string, method *uuid.UUID, now time.Time) error {
	if invoice.Origin != string(billing.InvoiceOriginSubscription) || invoice.PeriodEndsAt == nil {
		return errors.New("a renewal attempt pays a renewal invoice")
	}
	if invoice.Status != string(billing.InvoiceOpen) {
		return fmt.Errorf("renewal invoice %s is %s", invoice.ID, invoice.Status)
	}
	state, err := json.Marshal(map[string]any{"operation_id": operation.String()})
	if err != nil {
		return err
	}
	if err := q.CreateInvoiceAttempt(ctx, gen.CreateInvoiceAttemptParams{ID: operation, MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
		Rail: rail, Status: "processing", Amount: invoice.Total, Currency: invoice.Currency, ExpiresAt: *invoice.PeriodEndsAt, RailState: state, PspID: psp, Now: now}); err != nil {
		return err
	}
	if n, err := q.StartInvoiceAttempt(ctx, gen.StartInvoiceAttemptParams{MerchantID: invoice.MerchantID, ID: invoice.ID, AttemptID: operation, PaymentMethodID: method, PspID: psp, Now: now}); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("renewal invoice %s took no attempt", invoice.ID))
	}
	_, err = q.SetInvoicePending(ctx, gen.SetInvoicePendingParams{MerchantID: invoice.MerchantID, ID: invoice.ID, AttemptID: operation, PaymentStatus: string(billing.InvoicePaymentProcessing), Now: now})
	return err
}

// RenewalPaid completes the renewal invoice its collection paid, in the
// collection's completion transaction: numbered, linked to the payment the
// renewal recorded, with invoice.completed. A renewal whose subscription ended
// without receiving this paid period is void instead, with the refunded payment.
// keepCanceled means the lifecycle granted the period without resuming renewal.
// payment is nil when a reversal recorded none.
func RenewalPaid(ctx context.Context, d *db.DB, merchantID, invoiceID, attemptID uuid.UUID, payment *uuid.UUID, transactionID string, now time.Time, clock clockwork.Clock, keepCanceled bool) error {
	q := d.Gen(ctx)
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: invoiceID})
	if err != nil {
		return err
	}
	if payment != nil {
		n, err := q.LinkAcceptedRenewalPaymentInvoice(ctx, gen.LinkAcceptedRenewalPaymentInvoiceParams{MerchantID: merchantID, InvoiceID: invoiceID, PaymentID: *payment})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("renewal payment does not belong to this invoice")
		}
	}
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: merchantID, ID: attemptID, Status: "succeeded", PaymentID: payment, TransactionID: &transactionID, Now: now}); err != nil {
		return err
	}
	if invoice.Status != string(billing.InvoiceOpen) && invoice.Status != string(billing.InvoiceProcessing) {
		return nil
	}
	ended, err := renewalEnded(ctx, q, invoice)
	if err != nil {
		return err
	}
	if ended && !keepCanceled {
		return voidRenewal(ctx, d, &invoice, payment, now)
	}
	if payment == nil {
		return fmt.Errorf("renewal invoice %s was paid without a payment", invoiceID)
	}
	number, err := q.NextInvoiceNumber(ctx)
	if err != nil {
		return err
	}
	if n, err := q.SetInvoiceComplete(ctx, gen.SetInvoiceCompleteParams{MerchantID: merchantID, ID: invoiceID, Number: number, PaymentID: payment, Now: now}); err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("renewal invoice %s did not complete", invoiceID))
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceComplete), string(billing.InvoicePaymentSucceeded)
	lines, err := q.ListInvoiceLines(ctx, gen.ListInvoiceLinesParams{MerchantID: merchantID, InvoiceIds: []uuid.UUID{invoiceID}})
	if err != nil {
		return err
	}
	for _, line := range lines {
		if err := fundCreditLine(ctx, d, line, *payment, now, clock); err != nil {
			return err
		}
	}
	if err := books.ApplyInvoice(ctx, d, invoiceID, nil); err != nil {
		return err
	}
	if err := invoiceEvent(ctx, q, &invoice, "invoice.completed", now, map[string]any{"number": number, "payment_id": payment.String()}); err != nil {
		return err
	}
	return notificationinbox.WithdrawInvoice(ctx, d, invoice.CustomerID, invoice.ID)
}

// RenewalUnpaid ends a collection attempt that moved no money: the renewal
// invoice is open again, with why, or void once its subscription ended.
func RenewalUnpaid(ctx context.Context, d *db.DB, merchantID, invoiceID, attemptID uuid.UUID, failure billing.PaymentFailure, now time.Time) error {
	q := d.Gen(ctx)
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: merchantID, ID: attemptID, Status: "failed", Now: now}); err != nil {
		return err
	}
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: invoiceID})
	if err != nil {
		return err
	}
	if invoice.AttemptID == nil || *invoice.AttemptID != attemptID || (invoice.Status != string(billing.InvoiceOpen) && invoice.Status != string(billing.InvoiceProcessing)) {
		return nil
	}
	ended, err := renewalEnded(ctx, q, invoice)
	if err != nil {
		return err
	}
	if ended {
		return voidRenewal(ctx, d, &invoice, nil, now)
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	if _, err := q.SetInvoiceDeclined(ctx, gen.SetInvoiceDeclinedParams{MerchantID: merchantID, ID: invoiceID, AttemptID: attemptID, LastPaymentError: raw, Now: now}); err != nil {
		return err
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceOpen), string(billing.InvoicePaymentRequiresPaymentMethod)
	return invoiceEvent(ctx, q, &invoice, "invoice.payment_failed", now, map[string]any{"reason": failure.Reason})
}

// renewalEnded reports a renewal invoice whose subscription ended.
func renewalEnded(ctx context.Context, q *gen.Queries, invoice gen.BillingInvoice) (bool, error) {
	sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: invoice.MerchantID, ID: *invoice.SubscriptionID})
	if err != nil {
		return false, err
	}
	return sub.Status == "canceled" || sub.DeletedAt != nil, nil
}

// voidRenewal voids a live renewal invoice, with invoice.voided.
func voidRenewal(ctx context.Context, d *db.DB, invoice *gen.BillingInvoice, payment *uuid.UUID, now time.Time) error {
	q := d.Gen(ctx)
	n, err := q.VoidRenewalInvoice(ctx, gen.VoidRenewalInvoiceParams{MerchantID: invoice.MerchantID, ID: invoice.ID, PaymentID: payment, Now: now})
	if err != nil || n == 0 {
		return err
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceVoid), string(billing.InvoicePaymentRequiresPaymentMethod)
	extra := map[string]any{}
	if payment != nil {
		invoice.PaymentStatus = string(billing.InvoicePaymentSucceeded)
		extra["payment_id"] = payment.String()
	}
	if err := invoiceEvent(ctx, q, invoice, "invoice.voided", now, extra); err != nil {
		return err
	}
	return notificationinbox.WithdrawInvoice(ctx, d, invoice.CustomerID, invoice.ID)
}

// RenewalAwaitsCustomer marks the renewal invoice's payment as waiting for the
// customer's authentication; it reports whether it was not already.
func RenewalAwaitsCustomer(ctx context.Context, q *gen.Queries, merchantID, invoiceID, attemptID uuid.UUID, now time.Time) (bool, error) {
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: invoiceID})
	if err != nil {
		return false, err
	}
	if invoice.AttemptID == nil || *invoice.AttemptID != attemptID || invoice.PaymentStatus == string(billing.InvoicePaymentRequiresAction) {
		return false, nil
	}
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: merchantID, ID: attemptID, Status: "requires_action", Now: now}); err != nil {
		return false, err
	}
	n, err := q.SetInvoicePending(ctx, gen.SetInvoicePendingParams{MerchantID: merchantID, ID: invoiceID, AttemptID: attemptID, PaymentStatus: string(billing.InvoicePaymentRequiresAction), Now: now})
	if err != nil || n == 0 {
		return false, err
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceOpen), string(billing.InvoicePaymentRequiresAction)
	return true, invoiceEvent(ctx, q, &invoice, "invoice.requires_action", now, nil)
}
