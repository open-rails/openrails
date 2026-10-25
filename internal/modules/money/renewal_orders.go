package money

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// A collection is an attempt on its renewal order (payload OrderID): its
// completion settles the order in the same transaction.

// renewalOrderPaid settles the collection's renewal order with the payment
// the renewal recorded.
func (h *SubscriptionCollectionHandler) renewalOrderPaid(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, transactionID string) error {
	if p.OrderID == uuid.Nil {
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
	return orders.RenewalPaid(ctx, d, in.MerchantID, p.OrderID, in.ID, paid, transactionID, h.now())
}

// renewalOrderUnpaid returns the collection's renewal order to the customer
// with why no money moved.
func (h *SubscriptionCollectionHandler) renewalOrderUnpaid(ctx context.Context, d *db.DB, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, failure billing.PaymentFailure) error {
	if p.OrderID == uuid.Nil {
		return nil
	}
	return orders.RenewalUnpaid(ctx, d, in.MerchantID, p.OrderID, in.ID, failure, h.now())
}

// renewalAwaitsCustomer: the bank asks the customer to authenticate the
// renewal's payment. Its order awaits them (next_action authenticate, then
// confirm). A renewal the merchant charged tells them, once per attempt; one
// they are paying is in front of them.
func (h *SubscriptionCollectionHandler) renewalAwaitsCustomer(ctx context.Context, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload) error {
	if p.OrderID == uuid.Nil {
		return nil
	}
	return h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		changed, err := orders.RenewalAwaitsCustomer(ctx, d.Gen(ctx), in.MerchantID, p.OrderID, in.ID, h.now())
		if err != nil || !changed || p.Initiator == charge.InitiatorCustomer {
			return err
		}
		return subscriptions.QueuePaymentAuthenticationRequired(ctx, d, subscriptions.AuthenticationRequest{AttemptID: in.ID, OrderID: p.OrderID,
			CustomerID: p.Renewal.CustomerID, Items: p.Renewal.ProductName, Amount: p.Renewal.Amount, Currency: p.Renewal.Currency, At: h.now()})
	})
}
