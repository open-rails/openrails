package repo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
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

func (r *NotificationQueueRepo) Create(ctx context.Context, notification *models.NotificationQueue) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Q(ctx), uuid.Nil, notification.TenantSubjectID); err != nil {
		return err
	}
	res, err := r.db.Q(ctx).NewInsert().Model(notification).Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.NotificationQueue, error) {
	notification := new(models.NotificationQueue)
	if err := r.db.Q(ctx).NewSelect().Model(notification).Where("nq.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return notification, nil
}

func (r *NotificationQueueRepo) GetByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	notifications := []*models.NotificationQueue{}
	if err := r.db.Q(ctx).NewSelect().Model(&notifications).Where("nq.tenant_subject_id = ?", tsid).OrderExpr("nq.created_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	return notifications, nil
}

func (r *NotificationQueueRepo) GetUnseenByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	notifications := []*models.NotificationQueue{}
	if err := r.db.Q(ctx).NewSelect().Model(&notifications).Where("nq.tenant_subject_id = ?", tsid).Where("nq.seen = ?", false).OrderExpr("nq.created_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	return notifications, nil
}

func (r *NotificationQueueRepo) GetByEventType(ctx context.Context, eventType models.NotificationEventType) ([]*models.NotificationQueue, error) {
	notifications := []*models.NotificationQueue{}
	if err := r.db.Q(ctx).NewSelect().Model(&notifications).Where("nq.event_type = ?", eventType).OrderExpr("nq.created_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	return notifications, nil
}

func (r *NotificationQueueRepo) CountByUserAndEventSince(ctx context.Context, userID string, eventType models.NotificationEventType, since time.Time) (int, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return 0, err
	}
	var count int
	err = r.db.Q(ctx).NewSelect().
		Model((*models.NotificationQueue)(nil)).
		ColumnExpr("COUNT(*)").
		Where("nq.tenant_subject_id = ?", tsid).
		Where("nq.event_type = ?", eventType).
		Where("nq.created_at >= ?", since).
		Scan(ctx, &count)
	return count, err
}

func (r *NotificationQueueRepo) GetUsersWithPendingDigest(ctx context.Context, since time.Time) ([]string, error) {
	userIDs := []string{}
	err := r.db.Q(ctx).NewSelect().
		Model((*models.NotificationQueue)(nil)).
		ColumnExpr("DISTINCT nq.tenant_subject_id::text").
		Where("nq.event_type = ?", models.NotificationTranslationCompletedPendingDigest).
		Where("nq.created_at >= ?", since).
		Scan(ctx, &userIDs)
	if err != nil {
		return nil, err
	}
	return userIDs, nil
}

func (r *NotificationQueueRepo) GetPendingDigestForUser(ctx context.Context, userID string, since time.Time, limit int) ([]*models.NotificationQueue, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	items := []*models.NotificationQueue{}
	q := r.db.Q(ctx).NewSelect().Model(&items).
		Where("nq.tenant_subject_id = ?", tsid).
		Where("nq.event_type = ?", models.NotificationTranslationCompletedPendingDigest).
		Where("nq.created_at >= ?", since).
		OrderExpr("nq.created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *NotificationQueueRepo) MarkAsSeen(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.Q(ctx).NewUpdate().Model((*models.NotificationQueue)(nil)).Set("seen = ?", true).Where("nq.id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) Update(ctx context.Context, notification *models.NotificationQueue) error {
	res, err := r.db.Q(ctx).NewUpdate().Model(notification).WherePK().Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) Delete(ctx context.Context, id uuid.UUID) error {
	res, err := r.db.Q(ctx).NewDelete().Model((*models.NotificationQueue)(nil)).Where("nq.id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *NotificationQueueRepo) GetNotifications(ctx context.Context, opts query.QueryOptions[NotificationFilters]) ([]*models.NotificationQueue, int64, error) {
	notifications := []*models.NotificationQueue{}
	q := r.db.Q(ctx).NewSelect().Model(&notifications)

	if opts.Filters.UserID != "" {
		tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, opts.Filters.UserID)
		if err != nil {
			return nil, 0, err
		}
		q = q.Where("nq.tenant_subject_id = ?", tsid)
	}
	if opts.Filters.EventType != "" {
		q = q.Where("nq.event_type = ?", opts.Filters.EventType)
	}
	if opts.Filters.Seen != nil {
		q = q.Where("nq.seen = ?", *opts.Filters.Seen)
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}

	q = q.Limit(opts.GetLimit()).Offset(opts.GetOffset()).OrderExpr("nq.created_at DESC")

	if err := q.Scan(ctx); err != nil {
		return nil, 0, err
	}

	return notifications, int64(total), nil
}
