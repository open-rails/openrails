package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

// NotificationEventType represents the type of notification event
type NotificationEventType string

const (
	// Premium lifecycle notifications (3 types)
	NotificationPremiumStarted NotificationEventType = "premium_started" // (1) Premium started
	NotificationPremiumRenewed NotificationEventType = "premium_renewed" // (2) Premium renewed
	NotificationPremiumEnded   NotificationEventType = "premium_ended"   // (3) Premium ended

	// Payment method notifications (3 types)
	NotificationPaymentMethodFailed         NotificationEventType = "payment_method_failed"          // (4) Payment method failed
	NotificationPaymentMethodAutoUpdated    NotificationEventType = "payment_method_auto_updated"    // (5) Payment method auto-updated
	NotificationPaymentMethodUpdateRequired NotificationEventType = "payment_method_update_required" // (6) Payment method update required

	// One-off payment notifications
	NotificationOneOffPurchaseCompleted NotificationEventType = "one_off_purchase_completed" // (8) Solana or other one-off purchase completed

	// System notifications (1 type)
	NotificationSystemAlert NotificationEventType = "system_alert" // (7) Arbitrary system notifications

	// Credits notifications
	// NotificationLowBalance fires when an owner's available credit balance drops
	// below the credit type's configured low_balance_threshold (issue #240,
	// completing the deferred #116 alert). Data carries owner_id, tenant_id,
	// credit_type, available, and threshold.
	NotificationLowBalance NotificationEventType = "low_balance" // Credit balance below threshold

	// Translation notifications
	NotificationTranslationCompleted              NotificationEventType = "translation_completed"                // Voted translation completed (rate-limited)
	NotificationTranslationCompletedPendingDigest NotificationEventType = "translation_completed_pending_digest" // queued for weekly digest
	NotificationTranslationDigestSent             NotificationEventType = "translation_digest_sent"              // audit of digest sends
)

// NotificationQueue stores in-app notification attempts
// Used for rebill failures and other user notifications
type NotificationQueue struct {
	bun.BaseModel `bun:"table:billing.notification_queue,alias:nq"`

	ID        uuid.UUID             `bun:"id,pk,type:uuid" json:"id"`
	UserID    string                `bun:"user_id,notnull" json:"user_id"`
	EventType NotificationEventType `bun:"event_type,notnull" json:"event_type"`
	Data      map[string]any        `bun:"data,type:jsonb" json:"data,omitempty"`
	Seen      bool                  `bun:"seen,notnull,default:false" json:"seen"` // Whether user has seen this notification
	CreatedAt time.Time             `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// IsSeen checks if the notification has been seen by the user
func (nq *NotificationQueue) IsSeen() bool {
	return nq.Seen
}

// MarkAsSeen marks the notification as seen by the user
func (nq *NotificationQueue) MarkAsSeen() {
	nq.Seen = true
}

// IsExpiredForCleanup checks if the notification is old enough to be cleaned up
func (nq *NotificationQueue) IsExpiredForCleanup() bool {
	now := time.Now()

	// Seen notifications can be cleaned up after 90 days
	if nq.IsSeen() {
		return nq.CreatedAt.Before(now.Add(-90 * 24 * time.Hour))
	}

	// Unseen notifications are kept longer (180 days)
	return nq.CreatedAt.Before(now.Add(-180 * 24 * time.Hour))
}
