package repo

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
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

func notificationFromGen(n gen.BillingNotificationQueue) (*models.NotificationQueue, error) {
	m := &models.NotificationQueue{
		ID:              n.ID,
		TenantSubjectID: n.TenantSubjectID,
		EventType:       models.NotificationEventType(n.EventType),
		Seen:            n.Seen,
		CreatedAt:       n.CreatedAt,
	}
	if err := fromJSONB(n.Data, &m.Data, "notification_queue.data"); err != nil {
		return nil, err
	}
	return m, nil
}

func notificationsFromGen(rows []gen.BillingNotificationQueue) ([]*models.NotificationQueue, error) {
	out := make([]*models.NotificationQueue, 0, len(rows))
	for _, r := range rows {
		m, err := notificationFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *NotificationQueueRepo) Create(ctx context.Context, notification *models.NotificationQueue) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Qx(ctx), uuid.Nil, notification.TenantSubjectID); err != nil {
		return err
	}
	data, err := toJSONB(notification.Data)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreateNotification(ctx, gen.CreateNotificationParams{
		ID:              notification.ID,
		TenantSubjectID: notification.TenantSubjectID,
		EventType:       string(notification.EventType),
		Data:            data,
		Seen:            notification.Seen,
		CreatedAt:       notification.CreatedAt,
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
	return notificationFromGen(row)
}

func (r *NotificationQueueRepo) GetByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListNotificationsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return notificationsFromGen(rows)
}

func (r *NotificationQueueRepo) GetUnseenByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListUnseenNotificationsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return notificationsFromGen(rows)
}

func (r *NotificationQueueRepo) GetByEventType(ctx context.Context, eventType models.NotificationEventType) ([]*models.NotificationQueue, error) {
	rows, err := r.db.Gen(ctx).ListNotificationsByEventType(ctx, string(eventType))
	if err != nil {
		return nil, err
	}
	return notificationsFromGen(rows)
}

func (r *NotificationQueueRepo) CountByUserAndEventSince(ctx context.Context, userID string, eventType models.NotificationEventType, since time.Time) (int, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return 0, err
	}
	count, err := r.db.Gen(ctx).CountNotificationsByTenantSubjectEventSince(ctx, gen.CountNotificationsByTenantSubjectEventSinceParams{
		TenantSubjectID: tsid,
		EventType:       string(eventType),
		CreatedAt:       since,
	})
	return int(count), err
}

func (r *NotificationQueueRepo) GetUsersWithPendingDigest(ctx context.Context, since time.Time) ([]string, error) {
	return r.db.Gen(ctx).ListTenantSubjectsWithPendingDigest(ctx, gen.ListTenantSubjectsWithPendingDigestParams{
		EventType: string(models.NotificationTranslationCompletedPendingDigest),
		CreatedAt: since,
	})
}

func (r *NotificationQueueRepo) GetPendingDigestForUser(ctx context.Context, userID string, since time.Time, limit int) ([]*models.NotificationQueue, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.Gen(ctx).ListPendingDigestForTenantSubject(ctx, gen.ListPendingDigestForTenantSubjectParams{
		TenantSubjectID: tsid,
		EventType:       string(models.NotificationTranslationCompletedPendingDigest),
		CreatedAt:       since,
		PageLimit:       int32(min(limit, math.MaxInt32)),
	})
	if err != nil {
		return nil, err
	}
	return notificationsFromGen(rows)
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
	data, err := toJSONB(notification.Data)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdateNotification(ctx, gen.UpdateNotificationParams{
		ID:              notification.ID,
		TenantSubjectID: notification.TenantSubjectID,
		EventType:       string(notification.EventType),
		Data:            data,
		Seen:            notification.Seen,
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
		id, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, opts.Filters.UserID)
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
		TenantSubjectID: tsid,
		EventType:       eventType,
		Seen:            opts.Filters.Seen,
	})
	if err != nil {
		return nil, 0, err
	}
	rows, err := q.ListNotificationsFiltered(ctx, gen.ListNotificationsFilteredParams{
		TenantSubjectID: tsid,
		EventType:       eventType,
		Seen:            opts.Filters.Seen,
		PageLimit:       int32(min(opts.GetLimit(), math.MaxInt32)),
		PageOffset:      int32(min(opts.GetOffset(), math.MaxInt32)),
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

// NotificationFromGen exposes the gen -> model mapping for callers outside the
// repo (e.g. the admin operations handlers reading repair alerts).
func NotificationFromGen(n gen.BillingNotificationQueue) (*models.NotificationQueue, error) {
	return notificationFromGen(n)
}
