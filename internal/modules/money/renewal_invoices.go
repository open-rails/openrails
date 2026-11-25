package money

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A collection is an attempt on its renewal invoice (payload InvoiceID): its
// completion settles the invoice in the same transaction.

// renewalInvoicePaid settles the collection's renewal invoice with the payment
// the renewal recorded.
func (h *SubscriptionCollectionHandler) renewalInvoicePaid(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, transactionID string) error {
	if p.InvoiceID == uuid.Nil {
		return nil
	}
	// A reversed charge records no payment.
	var paid *uuid.UUID
	payment, err := payments.NewPaymentService(d, h.Clock).GetByPSPTransactionID(ctx, models.Rail(in.Rail), transactionID)
	switch {
	case err == nil:
		paid = &payment.ID
	case !db.IsNotFound(err):
		return err
	}
	return invoices.RenewalPaid(ctx, d, in.MerchantID, p.InvoiceID, in.ID, paid, transactionID, h.now(), h.Clock)
}

// renewalInvoiceUnpaid returns the collection's renewal invoice to the customer
// with why no money moved.
func (h *SubscriptionCollectionHandler) renewalInvoiceUnpaid(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, failure billing.PaymentFailure) error {
	if p.InvoiceID == uuid.Nil {
		return nil
	}
	return invoices.RenewalUnpaid(ctx, d, in.MerchantID, p.InvoiceID, in.ID, failure, h.now())
}

// renewalAwaitsCustomer: the bank asks the customer to authenticate the
// renewal's payment. Its invoice awaits them (next_action authenticate, then
// confirm). A renewal the merchant charged tells them, once per attempt; one
// they are paying is in front of them.
func (h *SubscriptionCollectionHandler) renewalAwaitsCustomer(ctx context.Context, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload) error {
	if p.InvoiceID == uuid.Nil {
		return nil
	}
	return h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		changed, err := invoices.RenewalAwaitsCustomer(ctx, d.Gen(ctx), in.MerchantID, p.InvoiceID, in.ID, h.now())
		if err != nil || !changed || p.Initiator == charge.InitiatorCustomer {
			return err
		}
		return subscriptions.QueuePaymentAuthenticationRequired(ctx, d, subscriptions.AuthenticationRequest{AttemptID: in.ID, InvoiceID: p.InvoiceID,
			CustomerID: p.Renewal.CustomerID, Items: p.Renewal.ProductName, Amount: p.Renewal.Amount, Currency: p.Renewal.Currency, At: h.now()})
	})
}
