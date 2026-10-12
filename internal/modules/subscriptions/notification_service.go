package subscriptions

import (
	"context"
	"errors"

	"github.com/google/uuid"
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
	params, err := r.createParams(ctx, notification)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreateNotification(ctx, params)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// CreateIfAbsent inserts notification once by its caller-supplied ID. An
// existing ID is success, which lets retrying fan-out jobs converge without
// duplicating deliveries that already succeeded.
func (r *NotificationService) CreateIfAbsent(ctx context.Context, notification *models.NotificationEvent) error {
	params, err := r.createParams(ctx, notification)
	if err != nil {
		return err
	}
	return r.db.Gen(ctx).CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
		ID:         params.ID,
		MerchantID: params.MerchantID,
		CustomerID: params.CustomerID,
		EventType:  params.EventType,
		Data:       params.Data,
		CreatedAt:  params.CreatedAt,
	})
}

func (r *NotificationService) createParams(ctx context.Context, notification *models.NotificationEvent) (gen.CreateNotificationParams, error) {
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, notification.CustomerID); err != nil {
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
