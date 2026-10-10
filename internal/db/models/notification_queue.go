package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
)

// NotificationEventType represents the type of notification event
type NotificationEventType string

const (
	// Premium lifecycle notifications
	NotificationPremiumStarted NotificationEventType = "premium_started"
	NotificationPremiumRenewed NotificationEventType = "premium_renewed"
	NotificationPremiumEnded   NotificationEventType = "premium_ended"

	// Payment method notifications
	NotificationPaymentMethodFailed         NotificationEventType = "payment_method_failed"
	NotificationPaymentMethodAutoUpdated    NotificationEventType = "payment_method_auto_updated"
	NotificationPaymentMethodUpdateRequired NotificationEventType = "payment_method_update_required"

	// One-off payment notifications
	NotificationOneOffPurchaseCompleted NotificationEventType = "one_off_purchase_completed" // a one-off purchase's receipt

	// Invoice collection stopped: nothing is canceled and the debt stands.
	// data.reason is schedule_exhausted (we gave up) or non_recoverable (the
	// issuer withdrew the mandate).
	NotificationInvoiceCollectionStopped NotificationEventType = "invoice_collection_stopped"

	// Arrears delinquency: the debt outlived the merchant's grace window.
	// Nothing is canceled or withdrawn; OpenRails refuses new spend and tells
	// the host, which owns any shutoff.
	NotificationAccountDelinquent        NotificationEventType = "account_delinquent"
	NotificationAccountDelinquencyClosed NotificationEventType = "account_delinquency_cleared"

	// Ordinary invoice lifecycle notifications, independent of onboarding.
	NotificationInvoiceIssued  NotificationEventType = "invoice_issued"
	NotificationInvoiceOverdue NotificationEventType = "invoice_overdue"

	// Translation notifications
	NotificationTranslationCompleted              NotificationEventType = "translation_completed"                // Voted translation completed (rate-limited)
	NotificationTranslationCompletedPendingDigest NotificationEventType = "translation_completed_pending_digest" // queued for weekly digest
	NotificationTranslationDigestSent             NotificationEventType = "translation_digest_sent"              // audit of digest sends

	// Fired when a subscription reprice is scheduled, not applied: the
	// card-network-required advance notice of a recurring amount change.
	NotificationSubscriptionRepriceScheduled NotificationEventType = "subscription_reprice_scheduled"

	// Fired when a plan migration is scheduled; distinct from
	// reprice_scheduled because the plan itself changes, not just the amount.
	NotificationSubscriptionPlanChangeScheduled NotificationEventType = "subscription_plan_change_scheduled"

	// Staff changed a subscription at the customer's request: its receipt
	// (amount charged now, if any) and the plan and seats from effective_at.
	NotificationSubscriptionChanged NotificationEventType = "subscription_changed"
)

// NotificationQueue is an in-app notification for a customer.
type NotificationQueue struct {
	ID uuid.UUID
	// CustomerID is the host's subject UUID within the merchant.
	CustomerID uuid.UUID
	EventType  NotificationEventType
	// Data is the typed event payload; it is stored as JSONB and served verbatim.
	Data      billing.NotificationData
	Seen      bool
	CreatedAt time.Time
}

// DataJSONB is the stored form of Data.
func (nq *NotificationQueue) DataJSONB() ([]byte, error) { return json.Marshal(nq.Data) }

// View is the wire shape of the row.
func (nq *NotificationQueue) View() billing.Notification {
	return billing.Notification{ID: billing.NotificationID(nq.ID), CustomerID: billing.CustomerID(nq.CustomerID), EventType: string(nq.EventType), Data: nq.Data, Seen: nq.Seen, CreatedAt: nq.CreatedAt}
}

// IsSeen checks if the notification has been seen by the user
func (nq *NotificationQueue) IsSeen() bool {
	return nq.Seen
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
