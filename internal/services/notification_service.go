package services

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/query"
)

// NotificationService handles notification storage, retrieval, and delivery.
// It combines DB operations with email delivery logic.
type NotificationService struct {
	repo         *repo.NotificationQueueRepo
	emailService *EmailService
}

// GetNotificationsFilters defines filters for notification queries
type GetNotificationsFilters struct {
	UserID    string `form:"user_id"`
	EventType string `form:"event_type"`
	Seen      *bool  `form:"seen"`
}

// NewNotificationService creates a new notification service.
// emailService can be nil and set later via SetEmailService.
func NewNotificationService(database *db.DB, emailService *EmailService) *NotificationService {
	return &NotificationService{
		repo:         repo.NewNotificationQueueRepo(database),
		emailService: emailService,
	}
}

// SetEmailService sets the email service for delivery operations.
// This allows delayed initialization to break circular dependencies.
func (s *NotificationService) SetEmailService(emailService *EmailService) {
	s.emailService = emailService
}

// ============================================================================
// DB Operations (formerly in NotificationQueueService)
// ============================================================================

func (s *NotificationService) Create(ctx context.Context, notification *models.NotificationQueue) error {
	return s.repo.Create(ctx, notification)
}

func (s *NotificationService) GetByID(ctx context.Context, id uuid.UUID) (*models.NotificationQueue, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *NotificationService) GetByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	return s.repo.GetByUserID(ctx, userID)
}

func (s *NotificationService) GetUnseenByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	return s.repo.GetUnseenByUserID(ctx, userID)
}

func (s *NotificationService) GetByEventType(ctx context.Context, eventType models.NotificationEventType) ([]*models.NotificationQueue, error) {
	return s.repo.GetByEventType(ctx, eventType)
}

func (s *NotificationService) CountByUserAndEventSince(ctx context.Context, userID string, eventType models.NotificationEventType, since time.Time) (int, error) {
	return s.repo.CountByUserAndEventSince(ctx, userID, eventType, since)
}

func (s *NotificationService) GetUsersWithPendingDigest(ctx context.Context, since time.Time) ([]string, error) {
	return s.repo.GetUsersWithPendingDigest(ctx, since)
}

func (s *NotificationService) GetPendingDigestForUser(ctx context.Context, userID string, since time.Time, limit int) ([]*models.NotificationQueue, error) {
	return s.repo.GetPendingDigestForUser(ctx, userID, since, limit)
}

func (s *NotificationService) MarkAsSeen(ctx context.Context, id uuid.UUID) error {
	return s.repo.MarkAsSeen(ctx, id)
}

func (s *NotificationService) Update(ctx context.Context, notification *models.NotificationQueue) error {
	return s.repo.Update(ctx, notification)
}

func (s *NotificationService) Delete(ctx context.Context, id uuid.UUID) error {
	return s.repo.Delete(ctx, id)
}

func (s *NotificationService) GetNotifications(ctx context.Context, queryOpts query.QueryOptions[GetNotificationsFilters]) ([]*models.NotificationQueue, int64, error) {
	repoFilters := repo.NotificationFilters{
		UserID:    queryOpts.Filters.UserID,
		EventType: models.NotificationEventType(queryOpts.Filters.EventType),
		Seen:      queryOpts.Filters.Seen,
	}

	repoOpts := query.QueryOptions[repo.NotificationFilters]{
		Filters:  repoFilters,
		Limit:    queryOpts.Limit,
		Offset:   queryOpts.Offset,
		Page:     queryOpts.Page,
		PageSize: queryOpts.PageSize,
		All:      queryOpts.All,
	}

	return s.repo.GetNotifications(ctx, repoOpts)
}

// ============================================================================
// Delivery Operations (create + send email)
// ============================================================================

// CreateAndDeliver creates a notification in the db and immediately sends external notifications
func (s *NotificationService) CreateAndDeliver(ctx context.Context, notification *models.NotificationQueue) error {
	// 1. Clean up obsolete notifications before creating the new one
	if err := s.cleanupObsoleteNotifications(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to cleanup obsolete notifications")
		// Continue with notification creation even if cleanup fails
	}

	// 2. Store in db
	if err := s.Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to create notification in db")
		return fmt.Errorf("failed to create notification: %w", err)
	}

	// 3. Immediately send external notifications (email, discord, etc.)
	if err := s.deliverExternalNotifications(ctx, notification); err != nil {
		// Log error but don't fail the entire operation since notification is already stored
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"notification_id": notification.ID,
			"user_id":         notification.UserID,
			"event_type":      notification.EventType,
		}).Error("failed to deliver external notifications")
	}

	return nil
}

// deliverExternalNotifications sends immediate external notifications (email, discord)
func (s *NotificationService) deliverExternalNotifications(ctx context.Context, notification *models.NotificationQueue) error {
	// Send email notification based on event type
	if err := s.sendEmailNotification(ctx, notification); err != nil {
		return fmt.Errorf("failed to send email notification: %w", err)
	}

	return nil
}

// DeliverEmail sends the appropriate email for an already-created notification
func (s *NotificationService) DeliverEmail(ctx context.Context, notification *models.NotificationQueue) error {
	return s.sendEmailNotification(ctx, notification)
}

// sendEmailNotification sends appropriate email based on notification type
func (s *NotificationService) sendEmailNotification(ctx context.Context, notification *models.NotificationQueue) error {
	if s.emailService == nil {
		log.WithContext(ctx).Debug("email service not available - skipping email notification")
		return nil
	}

	switch notification.EventType {
	case models.NotificationPremiumStarted:
		return s.emailService.SendSubscriptionConfirmed(ctx, notification.UserID)

	case models.NotificationPremiumRenewed:
		return s.emailService.SendSubscriptionRenewed(ctx, notification.UserID)

	case models.NotificationPremiumEnded:
		reason := subscriptions.PremiumEndReasonUnknown
		if notification.Data != nil {
			if r, ok := notification.Data["reason"].(string); ok {
				reason = subscriptions.ParsePremiumEndReason(r)
			}
		}
		return s.emailService.SendPremiumEnded(ctx, notification.UserID, reason)

	case models.NotificationPaymentMethodFailed:
		return s.emailService.SendPaymentFailed(ctx, notification.UserID)

	case models.NotificationOneOffPurchaseCompleted:
		if notification.Data == nil {
			log.WithContext(ctx).WithField("user_id", notification.UserID).Warn("one-off purchase notification missing data payload")
			return nil
		}

		email, _ := notification.Data["user_email"].(string)
		if email == "" {
			// Fallback to profiles lookup via email service
			if uname, mail, err := s.emailService.getUserEmail(ctx, notification.UserID); err == nil && mail != "" {
				_ = uname
				email = mail
			}
		}
		if email == "" {
			log.WithContext(ctx).WithField("user_id", notification.UserID).Warn("one-off purchase notification missing user email and profile lookup failed")
			return nil
		}

		amountFloat, _ := notification.Data["amount"].(float64) // JSON unmarshals numbers as float64
		currency, _ := notification.Data["currency"].(string)
		productName, _ := notification.Data["product_name"].(string)
		paymentMethod, _ := notification.Data["payment_method"].(string)

		return s.emailService.SendOneOffPurchaseReceipt(ctx, OneOffPurchaseEmailData{
			UserEmail:     email,
			Amount:        int64(amountFloat), // Convert to cents (int64)
			Currency:      currency,
			ProductName:   productName,
			PaymentMethod: paymentMethod,
			IsPremium:     true,
		})

	case models.NotificationPaymentMethodAutoUpdated:
		// No specific email for auto-updated payment methods - they're informational
		log.WithContext(ctx).Debug("payment method auto-updated - no email sent")
		return nil

	case models.NotificationPaymentMethodUpdateRequired:
		// This could trigger a payment method update email if we had one
		log.WithContext(ctx).Debug("payment method update required - no specific email template")
		return nil

	case models.NotificationSystemAlert:
		// System alerts are typically for admins, not user emails
		log.WithContext(ctx).Debug("system alert - no user email sent")
		return nil

	default:
		log.WithContext(ctx).WithField("event_type", notification.EventType).Warn("unknown notification event type for email delivery")
		return nil
	}
}

// cleanupObsoleteNotifications removes notifications that become irrelevant when user status improves
func (s *NotificationService) cleanupObsoleteNotifications(ctx context.Context, newNotification *models.NotificationQueue) error {
	var obsoleteEventTypes []models.NotificationEventType

	// Determine which notification types to clean up based on the new notification
	switch newNotification.EventType {
	case models.NotificationPremiumStarted, models.NotificationPremiumRenewed:
		// When premium starts or renews, remove any pending "premium ended" notifications
		obsoleteEventTypes = []models.NotificationEventType{
			models.NotificationPremiumEnded,
		}

	case models.NotificationPaymentMethodAutoUpdated:
		// When payment method is auto-updated, remove payment-related failure notifications
		obsoleteEventTypes = []models.NotificationEventType{
			models.NotificationPaymentMethodFailed,
			models.NotificationPaymentMethodUpdateRequired,
		}

	// Note: We could add more logic here for other success scenarios
	// For example, if we had a "payment succeeded" notification type, it could clean up payment failure notifications
	default:
		// No cleanup needed for this notification type
		return nil
	}

	// Remove obsolete unseen notifications for this user
	if len(obsoleteEventTypes) > 0 {
		cleanedCount, err := s.removeObsoleteNotifications(ctx, newNotification.UserID, obsoleteEventTypes)
		if err != nil {
			return fmt.Errorf("failed to remove obsolete notifications: %w", err)
		}

		if cleanedCount > 0 {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":        newNotification.UserID,
				"new_event_type": newNotification.EventType,
				"cleaned_count":  cleanedCount,
				"obsolete_types": obsoleteEventTypes,
			}).Info("cleaned up obsolete notifications due to status improvement")
		}
	}

	return nil
}

// removeObsoleteNotifications removes unseen notifications of specified types for a user
func (s *NotificationService) removeObsoleteNotifications(ctx context.Context, userID string, eventTypes []models.NotificationEventType) (int, error) {
	// Get unseen notifications for this user that match the obsolete types
	falseVal := false
	notifications, _, err := s.GetNotifications(ctx, query.QueryOptions[GetNotificationsFilters]{
		Filters: GetNotificationsFilters{
			UserID: userID,
			Seen:   &falseVal, // Only unseen notifications
		},
		Page:     1,
		PageSize: 1000, // Get a large batch to clean up
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get notifications for cleanup: %w", err)
	}

	cleanedCount := 0
	for _, notification := range notifications {
		// Check if this notification's type is in the obsolete list
		for _, obsoleteType := range eventTypes {
			if notification.EventType == obsoleteType {
				if err := s.Delete(ctx, notification.ID); err != nil {
					log.WithContext(ctx).WithError(err).WithFields(log.Fields{
						"notification_id": notification.ID,
						"event_type":      notification.EventType,
					}).Error("failed to delete obsolete notification")
				} else {
					cleanedCount++
				}
				break
			}
		}
	}

	return cleanedCount, nil
}
