package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/query"
)

// NotificationService handles notification storage, retrieval, and delivery.
// It combines DB operations with email delivery logic.
type NotificationService struct {
	repo         *NotificationQueueRepo
	emailService *EmailService
}

// NewNotificationService creates a new notification service.
// emailService can be nil and set later via SetEmailService.
func NewNotificationService(database *db.DB, emailService *EmailService) *NotificationService {
	return &NotificationService{
		repo:         NewNotificationQueueRepo(database),
		emailService: emailService,
	}
}

// SetEmailService sets the email service for delivery operations.
// This allows delayed initialization to break circular dependencies.
func (s *NotificationService) SetEmailService(emailService *EmailService) {
	s.emailService = emailService
}

func (s *NotificationService) Create(ctx context.Context, notification *models.NotificationQueue) error {
	return s.repo.Create(ctx, notification)
}

func (s *NotificationService) GetByID(ctx context.Context, id uuid.UUID) (*models.NotificationQueue, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *NotificationService) GetByUserID(ctx context.Context, userID string) ([]*models.NotificationQueue, error) {
	return s.repo.GetByUserID(ctx, userID)
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
	repoFilters := NotificationFilters{
		UserID:    queryOpts.Filters.UserID,
		EventType: models.NotificationEventType(queryOpts.Filters.EventType),
		Seen:      queryOpts.Filters.Seen,
	}

	repoOpts := query.QueryOptions[NotificationFilters]{
		Filters:  repoFilters,
		Limit:    queryOpts.Limit,
		Offset:   queryOpts.Offset,
		Page:     queryOpts.Page,
		PageSize: queryOpts.PageSize,
		All:      queryOpts.All,
	}

	return s.repo.GetNotifications(ctx, repoOpts)
}

// CreateAndDeliver creates a notification in the db and immediately sends external notifications.
func (s *NotificationService) CreateAndDeliver(ctx context.Context, notification *models.NotificationQueue) error {
	if err := s.cleanupObsoleteNotifications(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to cleanup obsolete notifications")
	}

	if err := s.Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).Error("failed to create notification in db")
		return fmt.Errorf("failed to create notification: %w", err)
	}

	if err := s.deliverExternalNotifications(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"notification_id": notification.ID,
			"user_id":         notification.CustomerID.String(),
			"event_type":      notification.EventType,
		}).Error("failed to deliver external notifications")
	}

	return nil
}

func (s *NotificationService) deliverExternalNotifications(ctx context.Context, notification *models.NotificationQueue) error {
	if err := s.DeliverEmail(ctx, notification); err != nil {
		return fmt.Errorf("failed to send email notification: %w", err)
	}

	return nil
}

// EmailEnabled reports whether an armed email service is attached.
func (s *NotificationService) EmailEnabled() bool {
	return s.emailService.IsEnabled()
}

// DeliverEmail sends the appropriate email for an already-created notification
// and stamps emailed_at on success (#789). No armed email service ⇒ the row is
// left undelivered (emailed_at NULL) so the sweep retries once email is wired;
// a rendered no-op (unsupported type, user without email) still stamps so the
// sweep never rescans it.
func (s *NotificationService) DeliverEmail(ctx context.Context, notification *models.NotificationQueue) error {
	if !s.EmailEnabled() {
		log.WithContext(ctx).Debug("email service not available - leaving notification undelivered")
		return nil
	}
	if err := s.sendEmailNotification(ctx, notification); err != nil {
		return err
	}
	if err := s.repo.MarkEmailed(ctx, notification.ID, time.Now().UTC()); err != nil {
		// The email already went out; a failed stamp means at most one duplicate
		// on the next sweep. Log, don't fail the delivery.
		log.WithContext(ctx).WithError(err).WithField("notification_id", notification.ID).
			Error("failed to stamp notification emailed_at")
	}
	return nil
}

func (s *NotificationService) sendEmailNotification(ctx context.Context, notification *models.NotificationQueue) error {
	if s.emailService == nil {
		log.WithContext(ctx).Debug("email service not available - skipping email notification")
		return nil
	}

	switch notification.EventType {
	case models.NotificationPremiumStarted:
		return s.emailService.SendSubscriptionConfirmed(ctx, notification.CustomerID.String())
	case models.NotificationPremiumRenewed:
		return s.emailService.SendSubscriptionRenewed(ctx, notification.CustomerID.String())
	case models.NotificationPremiumEnded:
		reason := PremiumEndReasonUnknown
		if notification.Data != nil {
			if r, ok := notification.Data["reason"].(string); ok {
				reason = ParsePremiumEndReason(r)
			}
		}
		if reason == PremiumEndReasonAccessEnded {
			// #789: subscription-row-free path; ended_at rides in the row data.
			endedAt := time.Now().UTC()
			if raw, ok := notification.Data["ended_at"].(string); ok {
				if t, err := time.Parse(time.RFC3339, raw); err == nil {
					endedAt = t
				}
			}
			return s.emailService.SendAccessEnded(ctx, notification.CustomerID.String(), endedAt)
		}
		return s.emailService.SendPremiumEnded(ctx, notification.CustomerID.String(), reason)
	case models.NotificationPaymentMethodFailed:
		return s.emailService.SendPaymentFailed(ctx, notification.CustomerID.String())
	case models.NotificationOneOffPurchaseCompleted:
		if notification.Data == nil {
			log.WithContext(ctx).WithField("user_id", notification.CustomerID.String()).Warn("one-off purchase notification missing data payload")
			return nil
		}

		email, _ := notification.Data["user_email"].(string)
		if email == "" {
			if uname, mail, err := s.emailService.getUserEmail(ctx, notification.CustomerID.String()); err == nil && mail != "" {
				_ = uname
				email = mail
			}
		}
		if email == "" {
			log.WithContext(ctx).WithField("user_id", notification.CustomerID.String()).Warn("one-off purchase notification missing user email and profile lookup failed")
			return nil
		}

		// amount_micros is a decimal INTEGER STRING, never a JSON number: a
		// JSONB round-trip decodes numbers as float64, and no monetary value
		// may pass through a float (#818). A malformed amount fails the
		// delivery rather than emailing a fabricated one.
		amountMicros, err := notificationAmountMicros(notification.Data["amount_micros"])
		if err != nil {
			return fmt.Errorf("one-off purchase notification amount: %w", err)
		}
		currency, _ := notification.Data["currency"].(string)
		productName, _ := notification.Data["product_name"].(string)
		paymentMethod, _ := notification.Data["payment_method"].(string)

		return s.emailService.SendOneOffPurchaseReceipt(ctx, OneOffPurchaseEmailData{
			UserEmail:     email,
			AmountMicros:  amountMicros,
			Currency:      currency,
			ProductName:   productName,
			PaymentMethod: paymentMethod,
			IsPremium:     true,
		})
	case models.NotificationPaymentMethodAutoUpdated:
		log.WithContext(ctx).Debug("payment method auto-updated - no email sent")
		return nil
	case models.NotificationPaymentMethodUpdateRequired:
		log.WithContext(ctx).Debug("payment method update required - no specific email template")
		return nil
	case models.NotificationSystemAlert:
		log.WithContext(ctx).Debug("system alert - no user email sent")
		return nil
	default:
		log.WithContext(ctx).WithField("event_type", notification.EventType).Warn("unknown notification event type for email delivery")
		return nil
	}
}

// notificationAmountMicros reads a money field out of a notification payload
// without ever touching a float. json.Number keeps the exact source text, so a
// producer that wrote a JSON integer still round-trips; anything else must be a
// decimal integer string.
func notificationAmountMicros(raw any) (int64, error) {
	switch v := raw.(type) {
	case nil:
		return 0, fmt.Errorf("missing amount_micros")
	case string:
		return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	case json.Number:
		return v.Int64()
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	default:
		return 0, fmt.Errorf("amount_micros must be a decimal integer string, got %T", raw)
	}
}

func (s *NotificationService) cleanupObsoleteNotifications(ctx context.Context, newNotification *models.NotificationQueue) error {
	var obsoleteEventTypes []models.NotificationEventType

	switch newNotification.EventType {
	case models.NotificationPremiumStarted, models.NotificationPremiumRenewed:
		obsoleteEventTypes = []models.NotificationEventType{models.NotificationPremiumEnded}
	case models.NotificationPaymentMethodAutoUpdated:
		obsoleteEventTypes = []models.NotificationEventType{
			models.NotificationPaymentMethodFailed,
			models.NotificationPaymentMethodUpdateRequired,
		}
	default:
		return nil
	}

	if len(obsoleteEventTypes) > 0 {
		cleanedCount, err := s.removeObsoleteNotifications(ctx, newNotification.CustomerID.String(), obsoleteEventTypes)
		if err != nil {
			return fmt.Errorf("failed to remove obsolete notifications: %w", err)
		}

		if cleanedCount > 0 {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":        newNotification.CustomerID.String(),
				"new_event_type": newNotification.EventType,
				"cleaned_count":  cleanedCount,
				"obsolete_types": obsoleteEventTypes,
			}).Info("cleaned up obsolete notifications due to status improvement")
		}
	}

	return nil
}

func (s *NotificationService) removeObsoleteNotifications(ctx context.Context, userID string, eventTypes []models.NotificationEventType) (int, error) {
	falseVal := false
	notifications, _, err := s.GetNotifications(ctx, query.QueryOptions[GetNotificationsFilters]{
		Filters: GetNotificationsFilters{
			UserID: userID,
			Seen:   &falseVal,
		},
		Page:     1,
		PageSize: 1000,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get notifications for cleanup: %w", err)
	}

	cleanedCount := 0
	for _, notification := range notifications {
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
