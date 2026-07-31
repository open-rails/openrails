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
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// LedgerRepairAlert describes a durable system alert for operator-led billing repair.
type LedgerRepairAlert struct {
	Provider          string
	Operation         string
	TransactionID     string
	UserID            string
	IdempotencyKey    string
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
	idempotencyKey := strings.TrimSpace(alert.IdempotencyKey)
	notificationID := uuidutil.NewV7()
	notification := &models.NotificationQueue{
		ID:        notificationID,
		EventType: models.NotificationSystemAlert,
		Data:      data,
		CreatedAt: now.UTC(),
	}
	// The alert is owned by this merchant's well-known system subject. Its ID is
	// merchant-derived because customers.id is globally unique under RLS (#889).
	if database != nil {
		merchantID, err := merchant.Require(ctx)
		if err != nil {
			return fmt.Errorf("resolve merchant for ledger repair alert: %w", err)
		}
		systemCustomerID := db.SystemCustomerID(merchantID.UUID())
		sysTSID, err := db.EnsureCustomerID(
			ctx,
			database.Qx(ctx),
			merchantID.UUID(),
			systemCustomerID.String(),
		)
		if err != nil {
			return fmt.Errorf("resolve system merchant subject for ledger repair alert: %w", err)
		}
		notification.CustomerID = sysTSID
		if idempotencyKey != "" {
			notification.ID = uuidutil.DeterministicID(
				uuidutil.DeterministicNamespace,
				"ledger_repair_alert",
				merchantID.String(),
				idempotencyKey,
			)
		}
	} else if idempotencyKey != "" {
		return fmt.Errorf("database is required for idempotent ledger repair alert")
	}
	var err error
	if idempotencyKey != "" {
		err = notificationService.CreateIfAbsent(ctx, notification)
	} else {
		err = notificationService.Create(ctx, notification)
	}
	if err != nil {
		return fmt.Errorf("create ledger repair alert: %w", err)
	}
	return nil
}
