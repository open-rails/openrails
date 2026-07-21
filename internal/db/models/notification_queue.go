package models

import (
	"time"

	"github.com/google/uuid"
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

	// Translation notifications
	NotificationTranslationCompleted              NotificationEventType = "translation_completed"                // Voted translation completed (rate-limited)
	NotificationTranslationCompletedPendingDigest NotificationEventType = "translation_completed_pending_digest" // queued for weekly digest
	NotificationTranslationDigestSent             NotificationEventType = "translation_digest_sent"              // audit of digest sends

	// #773: scheduling a subscription reprice fires this at SCHEDULE time (not
	// at apply time) — the card-network-required advance notice of an upcoming
	// recurring amount change (the disclosure hook #297 Phase A's MIT
	// compliance rides on).
	NotificationSubscriptionRepriceScheduled NotificationEventType = "subscription_reprice_scheduled"

	// #813: scheduling a plan MIGRATION fires this at SCHEDULE time — distinct
	// from reprice_scheduled because the disclosure content differs: the plan
	// (what the customer receives) is changing, not just the amount.
	NotificationSubscriptionPlanChangeScheduled NotificationEventType = "subscription_plan_change_scheduled"
)

// NotificationQueue stores in-app notification attempts
// Used for rebill failures and other user notifications
type NotificationQueue struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Additive during the hard-cut rollout; writers populate it and readers move to
	// it before user_id is dropped. Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID             `json:"customer_id,omitempty"`
	EventType  NotificationEventType `json:"event_type"`
	Data       map[string]any        `json:"data,omitempty"`
	Seen       bool                  `json:"seen"` // Whether user has seen this notification
	CreatedAt  time.Time             `json:"created_at"`
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
