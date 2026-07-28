package subscriptions

import (
	"context"
	"errors"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/query"
)

type NotificationFilters struct {
	UserID    string
	EventType models.NotificationEventType
	Seen      *bool
}

type NotificationQueueRepo struct {
	db *db.DB
}

func NewNotificationQueueRepo(d *db.DB) *NotificationQueueRepo { return &NotificationQueueRepo{db: d} }

func notificationsFromGen(rows []gen.OpenrailsNotificationQueue) ([]*models.NotificationQueue, error) {
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
	if err := db.EnsureCustomerRow(ctx, r.db.Qx(ctx), uuid.Nil, notification.CustomerID); err != nil {
		return err
	}
	data, err := models.ToJSONB(notification.Data)
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreateNotification(ctx, gen.CreateNotificationParams{
		ID:         notification.ID,
		MerchantID: tid.UUID(),
		CustomerID: notification.CustomerID,
		EventType:  string(notification.EventType),
		Data:       data,
		Seen:       notification.Seen,
		CreatedAt:  notification.CreatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
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

// MarkEmailed stamps emailed_at once (#789); already-stamped rows are a no-op.
func (r *NotificationQueueRepo) MarkEmailed(ctx context.Context, id uuid.UUID, at time.Time) error {
	_, err := r.db.Gen(ctx).MarkNotificationEmailed(ctx, gen.MarkNotificationEmailedParams{ID: id, EmailedAt: at})
	return err
}

func (r *NotificationQueueRepo) MarkAsSeen(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).MarkNotificationSeen(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) Update(ctx context.Context, notification *models.NotificationQueue) error {
	data, err := models.ToJSONB(notification.Data)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateNotification(ctx, gen.UpdateNotificationParams{
		ID:         notification.ID,
		CustomerID: notification.CustomerID,
		EventType:  string(notification.EventType),
		Data:       data,
		Seen:       notification.Seen,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).DeleteNotification(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) GetNotifications(ctx context.Context, opts query.QueryOptions[NotificationFilters]) ([]*models.NotificationQueue, int64, error) {
	var tsid *uuid.UUID
	if opts.Filters.UserID != "" {
		id, err := db.ResolveCustomerID(opts.Filters.UserID)
		if err != nil {
			return nil, 0, err
		}
		tsid = &id
	}
	var eventType *string
	if opts.Filters.EventType != "" {
		et := string(opts.Filters.EventType)
		eventType = &et
	}

	q := r.db.Gen(ctx)
	total, err := q.CountNotificationsFiltered(ctx, gen.CountNotificationsFilteredParams{
		CustomerID: tsid,
		EventType:  eventType,
		Seen:       opts.Filters.Seen,
	})
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](opts.GetLimit())
	offset32, _ := safecast.Convert[int32](opts.GetOffset())
	rows, err := q.ListNotificationsFiltered(ctx, gen.ListNotificationsFilteredParams{
		CustomerID: tsid,
		EventType:  eventType,
		Seen:       opts.Filters.Seen,
		PageLimit:  limit32,
		PageOffset: offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	notifications, err := notificationsFromGen(rows)
	if err != nil {
		return nil, 0, err
	}
	return notifications, total, nil
}
