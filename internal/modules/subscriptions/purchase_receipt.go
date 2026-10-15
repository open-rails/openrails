package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

// PurchaseReceipt is a paid one-off purchase: what was bought and what its
// payment took.
type PurchaseReceipt struct {
	PaymentID   uuid.UUID
	CustomerID  uuid.UUID
	Items       string
	Amount      int64
	Currency    string
	Rail        string
	OrderID     uuid.UUID
	OrderNumber int64
	PaidAt      time.Time
}

// QueuePurchaseReceipt records a one-off purchase fact in d, the transaction
// that records the payment. The receipt's id
// is the payment's, so a replayed settlement queues nothing new. A purchase
// that took no money has none.
func QueuePurchaseReceipt(ctx context.Context, d *db.DB, r PurchaseReceipt) error {
	if r.Amount <= 0 {
		return nil
	}
	if r.PaymentID == uuid.Nil || r.Items == "" {
		return fmt.Errorf("a receipt needs its payment and what was bought")
	}
	amount := r.Amount
	n := &models.NotificationEvent{
		ID:         uuid.NewSHA1(r.PaymentID, []byte(models.NotificationOneOffPurchaseCompleted)),
		CustomerID: r.CustomerID, EventType: models.NotificationOneOffPurchaseCompleted, CreatedAt: r.PaidAt.UTC(),
		Data: billing.NotificationData{Amount: &amount, Currency: r.Currency, ProductName: r.Items, Rail: r.Rail,
			PaymentID: billing.PaymentID(r.PaymentID), OrderID: billing.OrderID(r.OrderID), OrderNumber: r.OrderNumber},
	}
	if err := NewNotificationService(d).CreateIfAbsent(ctx, n); err != nil {
		return fmt.Errorf("queue receipt of payment %s: %w", r.PaymentID, err)
	}
	return nil
}

// AuthenticationRequest is a renewal payment the bank wants the customer to
// authenticate: one per attempt.
type AuthenticationRequest struct {
	AttemptID  uuid.UUID
	OrderID    uuid.UUID
	CustomerID uuid.UUID
	Items      string
	Amount     int64
	Currency   string
	At         time.Time
}

// QueuePaymentAuthenticationRequired tells the customer, in d's transaction,
// that a renewal's payment awaits their authentication on its order; the
// application consumes it independently. One fact per attempt.
func QueuePaymentAuthenticationRequired(ctx context.Context, d *db.DB, r AuthenticationRequest) error {
	if r.AttemptID == uuid.Nil || r.OrderID == uuid.Nil {
		return fmt.Errorf("an authentication notice needs its attempt and order")
	}
	amount := r.Amount
	n := &models.NotificationEvent{
		ID:         uuid.NewSHA1(r.AttemptID, []byte(models.NotificationPaymentAuthenticationRequired)),
		CustomerID: r.CustomerID, EventType: models.NotificationPaymentAuthenticationRequired, CreatedAt: r.At.UTC(),
		Data: billing.NotificationData{Amount: &amount, Currency: r.Currency, ProductName: r.Items, OrderID: billing.OrderID(r.OrderID), RetryStatus: billing.NotificationRetryStopped, RecoveryAction: billing.NotificationRecoveryAuthenticatePayment},
	}
	if err := NewNotificationService(d).CreateIfAbsent(ctx, n); err != nil {
		return fmt.Errorf("queue authentication notice of order %s: %w", r.OrderID, err)
	}
	return nil
}
