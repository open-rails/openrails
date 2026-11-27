package subscriptions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/billingfact"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// NotificationService records source facts and optional account-inbox mutations
// in its database transaction. Recipient inbox state belongs to the destination.
type NotificationService struct {
	db *db.DB
}

func NewNotificationService(d *db.DB) *NotificationService { return &NotificationService{db: d} }

// CreateIfAbsent records notification once by its caller-supplied ID, with its
// customer message (billingfact.Record). An existing ID is success, which lets
// retrying fan-out jobs converge without duplicating deliveries that already
// succeeded.
func (r *NotificationService) CreateIfAbsent(ctx context.Context, notification *models.NotificationEvent) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := r.db.NewWithPgxTx(tx)
		fact, err := NewNotificationService(txdb).fact(ctx, notification)
		if err != nil {
			return err
		}
		at, err := billingfact.Record(ctx, txdb.Gen(ctx), fact)
		if err != nil || at.IsZero() {
			return err // An old fact cannot become a newer notification revision.
		}
		notification.CreatedAt = at
		return publishInbox(ctx, txdb, tx, notification)
	})
}

func (r *NotificationService) fact(ctx context.Context, notification *models.NotificationEvent) (billingfact.Fact, error) {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, notification.CustomerID); err != nil {
		return billingfact.Fact{}, err
	}
	if err := r.membershipAccess(ctx, notification); err != nil {
		return billingfact.Fact{}, err
	}
	data, err := notification.DataJSONB()
	if err != nil {
		return billingfact.Fact{}, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return billingfact.Fact{}, err
	}
	return billingfact.Fact{
		ID:         notification.ID,
		MerchantID: tid.UUID(),
		CustomerID: notification.CustomerID,
		EventType:  string(notification.EventType),
		Data:       data,
		CreatedAt:  notification.CreatedAt,
	}, nil
}

// membershipAccess qualifies an end fact against the grants in the same
// transaction. A canceled renewal cannot claim that retained paid access ended.
func (r *NotificationService) membershipAccess(ctx context.Context, n *models.NotificationEvent) error {
	if n.EventType != models.NotificationPremiumEnded || n.Data.SubscriptionID.IsZero() {
		return nil
	}
	if n.CreatedAt.IsZero() {
		return errors.New("membership end notification requires its observed time")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	access, err := r.db.Gen(ctx).SubscriptionNotificationAccess(ctx, gen.SubscriptionNotificationAccessParams{
		MerchantID: mid.UUID(), CustomerID: n.CustomerID, SubscriptionID: n.Data.SubscriptionID.UUID().String(), AtTime: n.CreatedAt,
	})
	if err != nil {
		return err
	}
	if access.Retained || access.ClosedAt.IsZero() {
		n.EventType = models.NotificationSubscriptionCanceled
		n.Data.AccessEnded = false
		n.Data.EndedAt = nil
		if !access.RetainedUntil.IsZero() {
			at := access.RetainedUntil.UTC()
			n.Data.EndedAt = &at
		}
		return nil
	}
	at := access.ClosedAt.UTC()
	n.Data.AccessEnded, n.Data.EndedAt = true, &at
	return nil
}
