package subscriptions

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// NotificationService publishes source facts in its database transaction.
// It owns no inbox or delivery-channel state.
type NotificationService struct {
	db *db.DB
}

func NewNotificationService(d *db.DB) *NotificationService { return &NotificationService{db: d} }

func (r *NotificationService) Create(ctx context.Context, notification *models.NotificationEvent) error {
	return r.create(ctx, notification, false)
}

// CreateIfAbsent inserts notification once by its caller-supplied ID. An
// existing ID is success, which lets retrying fan-out jobs converge without
// duplicating deliveries that already succeeded.
func (r *NotificationService) CreateIfAbsent(ctx context.Context, notification *models.NotificationEvent) error {
	return r.create(ctx, notification, true)
}

func (r *NotificationService) create(ctx context.Context, notification *models.NotificationEvent, ifAbsent bool) error {
	return r.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := r.db.NewWithPgxTx(tx)
		params, err := NewNotificationService(txdb).createParams(ctx, notification)
		if err != nil {
			return err
		}
		query := `INSERT INTO billing.notification_events (id,merchant_id,customer_id,event_type,data,created_at)
VALUES($1,$2,$3,$4,$5,COALESCE(NULLIF($6::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),now()))`
		if ifAbsent {
			query += ` ON CONFLICT(merchant_id,id) DO NOTHING`
		}
		err = tx.QueryRow(ctx, query+` RETURNING created_at`, params.ID, params.MerchantID, params.CustomerID,
			params.EventType, params.Data, params.CreatedAt).Scan(&notification.CreatedAt)
		if ifAbsent && errors.Is(err, pgx.ErrNoRows) {
			return nil // An old fact cannot become a newer notification revision.
		}
		if err != nil {
			return err
		}
		return publishInbox(ctx, txdb, tx, notification)
	})
}

func (r *NotificationService) createParams(ctx context.Context, notification *models.NotificationEvent) (gen.CreateNotificationParams, error) {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, notification.CustomerID); err != nil {
		return gen.CreateNotificationParams{}, err
	}
	if err := r.membershipAccess(ctx, notification); err != nil {
		return gen.CreateNotificationParams{}, err
	}
	data, err := notification.DataJSONB()
	if err != nil {
		return gen.CreateNotificationParams{}, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return gen.CreateNotificationParams{}, err
	}
	return gen.CreateNotificationParams{
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
