package billing

import "time"

// ChangeSubscriptionParams changes a subscription's price, its seats, or both.
// PriceID names another price of the subscription's tier group; nil keeps the
// price. Quantity is the seats wanted, within the price's bounds; nil keeps
// them. Only a per-seat price has seats: a quantity for any other is refused
// with quantity_not_allowed. Reason is why staff make the change, required
// on a change (not a preview) and kept with it and any charge. IdempotencyKey
// identifies the change; reuse it until the change resolves.
type ChangeSubscriptionParams struct {
	PriceID        *PriceID `json:"price_id,omitempty"`
	Quantity       *int     `json:"quantity,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	IdempotencyKey string   `json:"-"`
}

// SubscriptionChange is the result of a subscription change. Status is
// succeeded, processing, requires_action or blocked; NextAction names the
// customer's step when it is requires_action. Effective is now (charged now)
// or period_end (applied by the next renewal, nothing charged now).
type SubscriptionChange struct {
	Status         string          `json:"status"`
	Effective      string          `json:"effective,omitempty"`
	PriceID        PriceID         `json:"price_id"`
	Quantity       *int            `json:"quantity"`
	Rail           string          `json:"rail"`
	SubscriptionID *SubscriptionID `json:"subscription_id,omitempty"`
	// NextAction is the customer's step when Status is requires_action: a
	// redirect to a provider page, or Solana transactions to sign.
	NextAction *NextAction `json:"next_action,omitempty"`
	// TransactionID is the provider's charge for a change that settled now.
	TransactionID *string    `json:"transaction_id"`
	Message       string     `json:"message,omitempty"`
	DelayedStart  *time.Time `json:"delayed_start,omitempty"`
	// AmountDueNow is what was charged now (0 for a change at period end);
	// NextChargeAmount and NextChargeDate are the next renewal. For a Stripe
	// provider subscription AmountDueNow is an estimate: Stripe finalizes the
	// proration.
	Currency         string     `json:"currency,omitempty"`
	AmountDueNow     int64      `json:"amount_due_now,string"`
	NextChargeAmount int64      `json:"next_charge_amount,string"`
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	// OperationID names the durable operation behind a change charged now or a
	// Stripe provider change. A processing answer (HTTP 202) carries it while
	// the outcome is unresolved, and a requires_action answer names the payment
	// the customer must authenticate (GET /v1/me/payment-operations/{id}/
	// authentication); the same Idempotency-Key replays the stored result.
	OperationID PaymentOperationID `json:"operation_id,omitzero"`
}

// SubscriptionChangePreview is what a change would charge now and at the next
// renewal, without making it.
type SubscriptionChangePreview struct {
	Effective        string     `json:"effective"` // now | period_end
	PriceID          PriceID    `json:"price_id"`
	Quantity         *int       `json:"quantity"`
	Rail             string     `json:"rail"`
	Currency         string     `json:"currency"`
	AmountDueNow     int64      `json:"amount_due_now,string"`
	NextChargeAmount int64      `json:"next_charge_amount,string"`
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	IsEstimate       bool       `json:"is_estimate"` // the rail finalizes the amount (Stripe provider changes)
	Message          string     `json:"message,omitempty"`
}

// Subscription-change refusals carry these StatusError.Code values.
const (
	// CodeSubscriptionChangeInFlight: another unresolved change owns the
	// subscription; metadata.operation_id names it.
	CodeSubscriptionChangeInFlight = "subscription_change_in_flight"
	// CodeSubscriptionChangeRefused: the provider definitively refused the
	// change.
	CodeSubscriptionChangeRefused = "subscription_change_refused"
	// CodeSubscriptionChangeIdempotencyConflict: the Idempotency-Key already
	// names a different change (another customer, subscription or target).
	CodeSubscriptionChangeIdempotencyConflict = "subscription_change_idempotency_conflict"
	// CodeSubscriptionChangeIdempotencyKeyRequired: a change needs a client
	// Idempotency-Key; it is the only way to read back a lost response.
	CodeSubscriptionChangeIdempotencyKeyRequired = "subscription_change_idempotency_key_required"
	// CodeSubscriptionChangeCycleUnknown: the target price has no positive
	// billing cycle, so an upgrade's new period is undefined.
	CodeSubscriptionChangeCycleUnknown = "subscription_change_cycle_unknown"
	// CodeSubscriptionChangePeriodUnknown: the subscription has no valid
	// current period to prorate against.
	CodeSubscriptionChangePeriodUnknown = "subscription_change_period_unknown"
	// CodeSubscriptionChangeCreditExceedsPrice: the current plan's unused value
	// is larger than the target (e.g. a long-cadence plan moving to a
	// short-cadence one early in its period); upgrades never forfeit credit.
	CodeSubscriptionChangeCreditExceedsPrice = "subscription_change_credit_exceeds_price"
	// CodeSubscriptionChangeRenewalDue: an engine-owned subscription's current
	// period has ended or its renewal is unresolved; the renewal settles first.
	CodeSubscriptionChangeRenewalDue = "subscription_change_renewal_due"
	// CodeSubscriptionChangeAlreadyScheduled: a different period-end change is
	// already scheduled on the subscription.
	CodeSubscriptionChangeAlreadyScheduled = "subscription_change_already_scheduled"
	// CodeSubscriptionChangeCadenceUnsupported: a provider-billed (NMI)
	// subscription keeps its schedule's next billing date, so its tier can
	// change only to a price of the same cadence.
	CodeSubscriptionChangeCadenceUnsupported = "subscription_change_cadence_unsupported"
	// CodeSubscriptionChangeRequiresLinkedPlan: the target price has no plan
	// on the subscription's PSP that this change can use (a named NMI plan
	// needs one of the same amount and cycle). Nothing was charged.
	CodeSubscriptionChangeRequiresLinkedPlan = "subscription_change_requires_linked_plan"
	// CodeSubscriptionChangeTargetInactive: the target price or its product is
	// archived.
	CodeSubscriptionChangeTargetInactive = "subscription_change_target_inactive"
	// CodeSubscriptionChangeUnsupportedOnRail: the subscription's rail cannot
	// make this change: seats on a provider-owned or Solana subscription, or a
	// CCBill downgrade.
	CodeSubscriptionChangeUnsupportedOnRail = "subscription_change_unsupported_on_rail"
	// CodeSubscriptionChangeProviderConflict: the provider's copy of the
	// subscription is missing or differs from OpenRails' (another price, a
	// provider schedule); reconcile it first.
	CodeSubscriptionChangeProviderConflict = "subscription_change_provider_conflict"
	// CodeStoredCredentialRequired: a merchant-initiated charge (a staff
	// change charged now) found no active agreement on the card for it.
	CodeStoredCredentialRequired = "stored_credential_required"
)
