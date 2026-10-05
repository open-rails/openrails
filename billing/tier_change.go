package billing

import "time"

// TierChange is the result of a tier change. Status is succeeded,
// processing, requires_action or blocked; NextAction names the customer's
// step when it is requires_action.
type TierChange struct {
	Status         string          `json:"status"`              // succeeded, processing, requires_action, blocked
	Action         string          `json:"action,omitempty"`    // upgrade, downgrade
	Effective      string          `json:"effective,omitempty"` // now (upgrade) | period_end (downgrade)
	PriceID        PriceID         `json:"price_id"`
	Rail           string          `json:"rail"`
	SubscriptionID *SubscriptionID `json:"subscription_id,omitempty"`
	// NextAction is the customer's step when Status is requires_action: a
	// redirect to a provider page, or Solana transactions to sign.
	NextAction *NextAction `json:"next_action,omitempty"`
	// TransactionID is the provider's charge for an upgrade that settled.
	TransactionID *string    `json:"transaction_id"`
	Message       string     `json:"message,omitempty"`       // User-friendly message
	DelayedStart  *time.Time `json:"delayed_start,omitempty"` // For scheduled downgrades
	// Money summary so the client can confirm/announce what actually happened.
	// AmountDueNow is what was charged immediately (0 for a scheduled downgrade);
	// NextChargeAmount/NextChargeDate describe the next renewal at the new price.
	// For Stripe upgrades AmountDueNow is the local Model B estimate (Stripe
	// finalizes the exact proration on its side), so treat it as approximate.
	Currency         string     `json:"currency,omitempty"`
	AmountDueNow     int64      `json:"amount_due_now,string"`
	NextChargeAmount int64      `json:"next_charge_amount,string"`
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	// OperationID names the durable operation behind an upgrade or a Stripe
	// tier change. A "processing" answer (HTTP 202) carries it while the
	// outcome is unresolved, and a "requires_action" answer names the payment
	// the customer must authenticate (GET /v1/me/payment-operations/{id}/
	// authentication); the same Idempotency-Key replays the stored result.
	OperationID PaymentOperationID `json:"operation_id,omitzero"`
}

// Tier-change refusals carry these StatusError.Code values.
const (
	// CodeTierChangeInFlight: another unresolved tier change owns the
	// subscription; metadata.operation_id names it.
	CodeTierChangeInFlight = "tier_change_in_flight"
	// CodeTierChangeRefused: the provider definitively refused the change.
	CodeTierChangeRefused = "tier_change_refused"
	// CodeTierChangeIdempotencyConflict: the Idempotency-Key already names a
	// different tier change (another customer, subscription or target).
	CodeTierChangeIdempotencyConflict = "tier_change_idempotency_conflict"
	// CodeTierChangeIdempotencyKeyRequired: a tier change needs a client
	// Idempotency-Key; it is the only way to read back a lost response.
	CodeTierChangeIdempotencyKeyRequired = "tier_change_idempotency_key_required"
	// CodeTierChangeCycleUnknown: the target price has no positive billing
	// cycle, so an upgrade's new period is undefined.
	CodeTierChangeCycleUnknown = "tier_change_cycle_unknown"
	// CodeTierChangePeriodUnknown: the subscription has no valid current
	// period to prorate against.
	CodeTierChangePeriodUnknown = "tier_change_period_unknown"
	// CodeTierChangeCreditExceedsPrice: the current plan's unused value is
	// larger than the target price (e.g. a long-cadence plan moving to a
	// short-cadence one early in its period); upgrades never forfeit credit.
	CodeTierChangeCreditExceedsPrice = "tier_change_credit_exceeds_price"
	// CodeTierChangeRenewalDue: an engine-owned subscription's current period
	// has ended or its renewal is unresolved; the renewal settles first.
	CodeTierChangeRenewalDue = "tier_change_renewal_due"
	// CodeTierChangeAlreadyScheduled: a different period-end change is already
	// scheduled on the subscription.
	CodeTierChangeAlreadyScheduled = "tier_change_already_scheduled"
	// CodeTierChangeCadenceUnsupported: a provider-billed (NMI) subscription
	// keeps its schedule's next billing date, so its tier can change only to a
	// price of the same cadence.
	CodeTierChangeCadenceUnsupported = "tier_change_cadence_unsupported"
	// CodeTierChangeRequiresLinkedPlan: the provider-billed (NMI) schedule is
	// on a named NMI plan, which NMI changes only by switching plans, and the
	// target price has no linked NMI plan on this account matching its amount
	// and cycle. Nothing was charged.
	CodeTierChangeRequiresLinkedPlan = "tier_change_requires_linked_plan"
)

type TierChangePreview struct {
	Action           string     `json:"action"` // upgrade | downgrade
	PriceID          PriceID    `json:"price_id"`
	Rail             string     `json:"rail"`
	Currency         string     `json:"currency"`
	AmountDueNow     int64      `json:"amount_due_now,string"`     // native units charged immediately (0 for downgrade)
	NextChargeAmount int64      `json:"next_charge_amount,string"` // native units at next renewal (new plan price)
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	Effective        string     `json:"effective"`   // "now" (upgrade) | "period_end" (downgrade)
	IsEstimate       bool       `json:"is_estimate"` // true when the rail finalizes the exact amount (Stripe upgrades)
	Message          string     `json:"message,omitempty"`
}

// ChangeTierParams moves a subscription to another price of its tier group.
// IdempotencyKey identifies the change; reuse it until the change resolves.
type ChangeTierParams struct {
	PriceID        PriceID `json:"price_id"`
	IdempotencyKey string  `json:"-"`
}
