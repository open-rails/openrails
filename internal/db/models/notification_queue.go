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
	// The bank asks the customer to authenticate a renewal's payment: its
	// order (data.order_id) awaits them.
	NotificationPaymentAuthenticationRequired NotificationEventType = "payment_authentication_required"

	// One-off payment notifications
	NotificationOneOffPurchaseCompleted NotificationEventType = "one_off_purchase_completed" // a one-off purchase's receipt

	// A usage order's collection stopped: nothing is canceled and the debt
	// stands. data.reason is schedule_exhausted (we gave up) or non_recoverable
	// (the issuer withdrew the mandate).
	NotificationOrderCollectionStopped NotificationEventType = "order_collection_stopped"

	// Arrears delinquency: the debt outlived the merchant's grace window.
	// Nothing is canceled or withdrawn; OpenRails refuses new spend and tells
	// the host, which owns any shutoff.
	NotificationAccountDelinquent        NotificationEventType = "account_delinquent"
	NotificationAccountDelinquencyClosed NotificationEventType = "account_delinquency_cleared"

	// The customer revoked its agreement to be charged for usage in a
	// currency (data.currency, effective_at; amount_due and order_id: what it
	// still owes, which it pays itself).
	NotificationAgreementRevoked NotificationEventType = "agreement_revoked"
	// A usage order charged under the customer's collection agreement: its
	// receipt (data.order_id, amount, currency, payment_id).
	NotificationUsageCharged NotificationEventType = "usage_charged"

	// A usage order with an amount due, and one past its due date.
	NotificationOrderDue     NotificationEventType = "order_due"
	NotificationOrderOverdue NotificationEventType = "order_overdue"

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

// NotificationQueue is a customer billing fact written in the domain transaction.
type NotificationQueue struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	CustomerID uuid.UUID
	EventType  NotificationEventType
	Data       billing.NotificationData
	CreatedAt  time.Time
}

func (n *NotificationQueue) DataJSONB() ([]byte, error) { return json.Marshal(n.Data) }

func (n *NotificationQueue) View() billing.NotificationEvent {
	data, _ := json.Marshal(n.Data)
	return billing.NotificationEvent{ID: billing.NotificationID(n.ID), MerchantID: billing.MerchantID(n.MerchantID), CustomerID: billing.CustomerID(n.CustomerID),
		EventType: string(n.EventType), Data: data, CreatedAt: n.CreatedAt, ActionRequested: billing.CustomerNotificationActionRequested(string(n.EventType)), Optional: billing.CustomerNotificationOptional(string(n.EventType), n.Data)}
}
