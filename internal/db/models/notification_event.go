package models

import (
	"encoding/json"
	"slices"
	"strings"
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
	// Recurrence stopped; paid access may remain. This is distinct from access loss.
	NotificationSubscriptionCanceled NotificationEventType = "subscription_canceled"

	// Payment method notifications
	NotificationPaymentMethodFailed         NotificationEventType = "payment_method_failed"
	NotificationPaymentMethodAutoUpdated    NotificationEventType = "payment_method_auto_updated"
	NotificationPaymentMethodUpdateRequired NotificationEventType = "payment_method_update_required"
	// The bank asks the customer to authenticate a renewal's payment: its
	// invoice (data.invoice_id) awaits them.
	NotificationPaymentAuthenticationRequired NotificationEventType = "payment_authentication_required"

	// One-off payment notifications
	NotificationOneOffPurchaseCompleted NotificationEventType = "one_off_purchase_completed" // a one-off purchase's receipt

	// A refund of the customer's payment settled: its receipt (data.amount
	// refunded, payment_id the refund, refunded_payment_id the charge, the
	// invoice and what it bought). One per refund.
	NotificationRefundCompleted NotificationEventType = "refund_completed"

	// A usage invoice's collection stopped: nothing is canceled and the debt
	// stands. data.reason is schedule_exhausted (we gave up) or non_recoverable
	// (the issuer withdrew the mandate).
	NotificationInvoiceCollectionStopped NotificationEventType = "invoice_collection_stopped"

	// Arrears delinquency: the debt outlived the merchant's grace window.
	// Nothing is canceled or withdrawn; OpenRails refuses new spend and tells
	// the host, which owns any shutoff.
	NotificationAccountDelinquent        NotificationEventType = "account_delinquent"
	NotificationAccountDelinquencyClosed NotificationEventType = "account_delinquency_cleared"

	// The customer revoked its agreement to be charged for usage in a
	// currency (data.currency, effective_at; amount_due and invoice_id: what it
	// still owes, which it pays itself).
	NotificationAgreementRevoked NotificationEventType = "agreement_revoked"
	// A usage invoice charged under the customer's collection agreement: its
	// receipt (data.invoice_id, amount, currency, payment_id).
	NotificationUsageCharged NotificationEventType = "usage_charged"

	// A watched balance went under its low line, or its capacity reached
	// zero: once per crossing (data.currency, available_amount, owed_amount,
	// low_balance_amount, runway_ends_at).
	NotificationBalanceLow      NotificationEventType = "balance_low"
	NotificationBalanceDepleted NotificationEventType = "balance_depleted"

	// A usage invoice with an amount due, and one past its due date.
	NotificationInvoiceDue     NotificationEventType = "invoice_due"
	NotificationInvoiceOverdue NotificationEventType = "invoice_overdue"

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

// NotificationEvent is a customer billing fact written in the domain transaction.
type NotificationEvent struct {
	ID         uuid.UUID
	MerchantID uuid.UUID
	CustomerID uuid.UUID
	EventType  NotificationEventType
	Data       billing.NotificationData
	CreatedAt  time.Time
}

func (n *NotificationEvent) DataJSONB() ([]byte, error) { return json.Marshal(n.Data) }

// Facts recorded before invoices were named keep their type and data keys: the
// rows are immutable, so readers rename them.
var (
	legacyNotificationTypes = map[string]NotificationEventType{
		"order_due": NotificationInvoiceDue, "order_overdue": NotificationInvoiceOverdue, "order_collection_stopped": NotificationInvoiceCollectionStopped,
	}
	legacyNotificationKeys = map[string]string{"order_id": "invoice_id", "order_number": "invoice_number", "overdue_orders": "overdue_invoices"}
)

// NotificationType is a stored fact's type as it is named now.
func NotificationType(stored string) NotificationEventType {
	if t, ok := legacyNotificationTypes[stored]; ok {
		return t
	}
	return NotificationEventType(stored)
}

// StoredNotificationTypes are the spellings facts of these types are stored
// under, earlier ones included.
func StoredNotificationTypes(types ...NotificationEventType) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, string(t))
	}
	for stored, t := range legacyNotificationTypes {
		if slices.Contains(types, t) {
			out = append(out, stored)
		}
	}
	return out
}

// DecodeNotificationData reads a stored fact's data, renaming the keys of one
// recorded before invoices were named (an order_id is ord_ prefixed).
func DecodeNotificationData(raw []byte, into *billing.NotificationData) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	renamed := false
	for stored, key := range legacyNotificationKeys {
		v, ok := fields[stored]
		if !ok {
			continue
		}
		var id string
		if stored == "order_id" && json.Unmarshal(v, &id) == nil {
			if rest, ok := strings.CutPrefix(id, "ord_"); ok {
				v, _ = json.Marshal("in_" + rest)
			}
		}
		delete(fields, stored)
		fields[key], renamed = v, true
	}
	if renamed {
		var err error
		if raw, err = json.Marshal(fields); err != nil {
			return err
		}
	}
	return json.Unmarshal(raw, into)
}
