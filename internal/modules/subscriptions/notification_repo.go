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

type NotificationQueueRepo struct {
	db *db.DB
}

func NewNotificationQueueRepo(d *db.DB) *NotificationQueueRepo { return &NotificationQueueRepo{db: d} }

func notificationsFromGen(rows []gen.BillingNotificationEvent) ([]*models.NotificationQueue, error) {
	out := make([]*models.NotificationQueue, 0, len(rows))
	for _, r := range rows {
		m, err := models.NotificationFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *NotificationQueueRepo) Create(ctx context.Context, notification *models.NotificationQueue) error {
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
func (r *NotificationQueueRepo) CreateIfAbsent(ctx context.Context, notification *models.NotificationQueue) error {
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

func (r *NotificationQueueRepo) createParams(ctx context.Context, notification *models.NotificationQueue) (gen.CreateNotificationParams, error) {
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

func (r *NotificationQueueRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.NotificationQueue, error) {
	row, err := r.db.Gen(ctx).GetNotificationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.NotificationFromGen(row)
}

func (r *NotificationQueueRepo) GetByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListNotificationsByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return notificationsFromGen(rows)
}
