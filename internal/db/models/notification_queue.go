package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
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

	// Invoice / arrears collection (or#828). The subscription analogue is
	// premium_ended, which is wrong for an invoice: nothing was cancelled and
	// no access was withdrawn — we stopped ATTEMPTING to collect, and the debt
	// stands. data.reason is schedule_exhausted (we gave up) or
	// non_recoverable (the issuer withdrew the mandate).
	NotificationInvoiceCollectionStopped NotificationEventType = "invoice_collection_stopped"

	// Arrears delinquency (or#878). The TIME axis, not the card axis: the debt
	// has outlived the merchant's grace window. Nothing is cancelled and no
	// entitlement is withdrawn — OpenRails refuses new spend and tells the host,
	// which owns whatever shutoff its product needs.
	NotificationAccountDelinquent        NotificationEventType = "account_delinquent"
	NotificationAccountDelinquencyClosed NotificationEventType = "account_delinquency_cleared"

	// Ordinary invoice lifecycle notifications, independent of onboarding.
	NotificationInvoiceIssued  NotificationEventType = "invoice_issued"
	NotificationInvoiceOverdue NotificationEventType = "invoice_overdue"

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
	ID uuid.UUID
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// The ID is the host subject UUID within MerchantID; customers stores issuer metadata.
	CustomerID uuid.UUID
	EventType  NotificationEventType
	// Data is the typed event payload; it is stored as JSONB and served verbatim.
	Data      openrails.NotificationData
	Seen      bool
	CreatedAt time.Time
}

// DataJSONB is the stored form of Data.
func (nq *NotificationQueue) DataJSONB() ([]byte, error) { return json.Marshal(nq.Data) }

// View is the wire shape of the row.
func (nq *NotificationQueue) View() openrails.Notification {
	return openrails.Notification{ID: nq.ID, CustomerID: openrails.CustomerID(nq.CustomerID).String(), EventType: string(nq.EventType), Data: nq.Data, Seen: nq.Seen, CreatedAt: nq.CreatedAt}
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
