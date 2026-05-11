package webhooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

type ledgerRepairAlert struct {
	Provider          string
	Operation         string
	TransactionID     string
	UserID            string
	OriginalPaymentID *uuid.UUID
	SubscriptionID    *uuid.UUID
	Err               error
	Metadata          map[string]any
}

func recordLedgerRepairAlert(ctx context.Context, notificationService *subscriptions.NotificationService, database *db.DB, now time.Time, alert ledgerRepairAlert) error {
	if notificationService == nil {
		if database == nil {
			return fmt.Errorf("notification service is required for ledger repair alert")
		}
		notificationService = subscriptions.NewNotificationService(database, nil)
	}

	userID := strings.TrimSpace(alert.UserID)

	data := map[string]any{
		"kind":           "billing_ledger_repair_required",
		"provider":       strings.TrimSpace(alert.Provider),
		"operation":      strings.TrimSpace(alert.Operation),
		"transaction_id": strings.TrimSpace(alert.TransactionID),
	}
	if userID != "" {
		data["affected_user_id"] = userID
	}
	if alert.Err != nil {
		data["error"] = alert.Err.Error()
	}
	if alert.OriginalPaymentID != nil {
		data["original_payment_id"] = alert.OriginalPaymentID.String()
	}
	if alert.SubscriptionID != nil {
		data["subscription_id"] = alert.SubscriptionID.String()
	}
	for key, value := range alert.Metadata {
		if strings.TrimSpace(key) != "" {
			data[key] = value
		}
	}

	if now.IsZero() {
		now = time.Now()
	}
	notification := &models.NotificationQueue{
		ID:        uuid.New(),
		UserID:    "system",
		EventType: models.NotificationSystemAlert,
		Data:      data,
		CreatedAt: now.UTC(),
	}
	if err := notificationService.Create(ctx, notification); err != nil {
		return fmt.Errorf("create ledger repair alert: %w", err)
	}
	return nil
}
