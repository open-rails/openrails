package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// NotificationEmailKind is the job that emails one queued notification.
const NotificationEmailKind = "openrails.notification_email"

// NotificationEmailArgs names the notification a job emails.
type NotificationEmailArgs struct {
	MerchantID     uuid.UUID `json:"merchant_id"`
	NotificationID uuid.UUID `json:"notification_id"`
}

func (NotificationEmailArgs) Kind() string { return NotificationEmailKind }

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
	OrderNumber string
	PaidAt      time.Time
}

// QueuePurchaseReceipt queues a one-off purchase's receipt and the job that
// emails it in d, the transaction that records the payment. The receipt's id
// is the payment's, so a replayed settlement queues nothing new. A purchase
// that took no money has none.
func QueuePurchaseReceipt(ctx context.Context, d *db.DB, r PurchaseReceipt) error {
	if r.Amount <= 0 {
		return nil
	}
	if r.PaymentID == uuid.Nil || r.Items == "" {
		return fmt.Errorf("a receipt needs its payment and what was bought")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	amount := r.Amount
	n := &models.NotificationQueue{
		ID:         uuid.NewSHA1(r.PaymentID, []byte(models.NotificationOneOffPurchaseCompleted)),
		CustomerID: r.CustomerID, EventType: models.NotificationOneOffPurchaseCompleted, CreatedAt: r.PaidAt.UTC(),
		Data: billing.NotificationData{Amount: &amount, Currency: r.Currency, ProductName: r.Items, Rail: r.Rail,
			PaymentID: billing.PaymentID(r.PaymentID), OrderID: billing.OrderID(r.OrderID), OrderNumber: r.OrderNumber},
	}
	if err := NewNotificationQueueRepo(d).CreateIfAbsent(ctx, n); err != nil {
		return fmt.Errorf("queue receipt of payment %s: %w", r.PaymentID, err)
	}
	return d.InsertRiverJob(ctx, NotificationEmailArgs{MerchantID: mid.UUID(), NotificationID: n.ID},
		&river.InsertOpts{Queue: "billing", UniqueOpts: river.UniqueOpts{ByArgs: true}})
}
