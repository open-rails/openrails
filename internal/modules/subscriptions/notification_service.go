package subscriptions

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

// NotificationService records immutable customer billing facts; applications
// consume them independently and own all delivery and presentation policy.
type NotificationService struct{ repo *NotificationQueueRepo }

func NewNotificationService(database *db.DB) *NotificationService {
	return &NotificationService{repo: NewNotificationQueueRepo(database)}
}
func (s *NotificationService) Create(ctx context.Context, n *models.NotificationQueue) error {
	return s.repo.Create(ctx, n)
}
func (s *NotificationService) CreateIfAbsent(ctx context.Context, n *models.NotificationQueue) error {
	return s.repo.CreateIfAbsent(ctx, n)
}
func (s *NotificationService) GetByID(ctx context.Context, id uuid.UUID) (*models.NotificationQueue, error) {
	return s.repo.GetByID(ctx, id)
}
func (s *NotificationService) GetByUserID(ctx context.Context, id string) ([]*models.NotificationQueue, error) {
	return s.repo.GetByUserID(ctx, id)
}
