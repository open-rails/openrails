package subscriptions

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/query"
)

type GetNotificationsFilters struct {
	UserID    string `form:"user_id"`
	EventType string `form:"event_type"`
	Seen      *bool  `form:"seen"`
}

type NotificationStore interface {
	Create(context.Context, *models.NotificationQueue) error
	GetByID(context.Context, uuid.UUID) (*models.NotificationQueue, error)
	Update(context.Context, *models.NotificationQueue) error
	GetNotifications(context.Context, query.QueryOptions[GetNotificationsFilters]) ([]*models.NotificationQueue, int64, error)
}

type NotificationEmailSender interface {
	DeliverEmail(context.Context, *models.NotificationQueue) error
}

type AdminCancellationLogger interface {
	LogAdminSubscriptionCancellation(context.Context, *models.Subscription, string, time.Time) error
}

type LifecycleEventLogger interface {
	LogLifecycleChargeSuccess(context.Context, *models.Subscription, models.Processor, string, int64, string, time.Time, map[string]interface{}) error
	LogLifecycleCancellation(context.Context, uuid.UUID, string, models.Processor, models.CancelType, bool, time.Time) error
	LogLifecycleFailure(context.Context, uuid.UUID, string, models.Processor, models.SubscriptionStatus, *string, *string, time.Time) error
}
