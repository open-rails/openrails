package webhooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// LedgerRepairAlert describes a durable system alert for operator-led billing repair.
type LedgerRepairAlert struct {
	Provider          string
	Operation         string
	TransactionID     string
	UserID            string
	OriginalPaymentID *uuid.UUID
	SubscriptionID    *uuid.UUID
	Err               error
	Metadata          map[string]any
}

// RecordLedgerRepairAlert stores a system notification visible through admin repair-alert endpoints.
func RecordLedgerRepairAlert(ctx context.Context, notificationService *subscriptions.NotificationService, database *db.DB, now time.Time, alert LedgerRepairAlert) error {
	return recordLedgerRepairAlert(ctx, notificationService, database, now, ledgerRepairAlert(alert))
}

type ledgerRepairAlert LedgerRepairAlert

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
		ID:        uuidutil.NewV7(),
		EventType: models.NotificationSystemAlert,
		Data:      data,
		CreatedAt: now.UTC(),
	}
	// The alert is owned by the well-known system tenant subject
	// (repo.SystemCustomerID) — materialize its row through the normal
	// self-issuer path like any other UUID subject (#364).
	if database != nil {
		sysTSID, err := repo.EnsureCustomerID(ctx, database.Qx(ctx), uuid.Nil, repo.SystemCustomerID.String())
		if err != nil {
			return fmt.Errorf("resolve system tenant subject for ledger repair alert: %w", err)
		}
		notification.CustomerID = sysTSID
	}
	if err := notificationService.Create(ctx, notification); err != nil {
		return fmt.Errorf("create ledger repair alert: %w", err)
	}
	return nil
}
