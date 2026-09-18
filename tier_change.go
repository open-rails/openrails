package openrails

import "time"

type TierChangeResponse struct {
	Object         string                         `json:"object"`                    // "tier_change"
	Status         string                         `json:"status"`                    // succeeded, processing, requires_action, blocked
	Mode           string                         `json:"mode"`                      // "tier_change"
	Action         string                         `json:"action,omitempty"`          // upgrade, downgrade
	PriceID        PriceID                        `json:"price_id"`                  // Target price ID
	URL            string                         `json:"url,omitempty"`             // Hosted redirect URL when required
	Payment        CheckoutSessionPaymentResponse `json:"payment"`                   // Rail info
	SubscriptionID *SubscriptionID                `json:"subscription_id,omitempty"` // Affected subscription
	NextAction     *CheckoutSessionNextAction     `json:"next_action,omitempty"`     // For redirects
	Message        string                         `json:"message,omitempty"`         // User-friendly message
	DelayedStart   *time.Time                     `json:"delayed_start,omitempty"`   // For scheduled downgrades
	// Money summary so the client can confirm/announce what actually happened.
	// AmountDueNow is what was charged immediately (0 for a scheduled downgrade);
	// NextChargeAmount/NextChargeDate describe the next renewal at the new price.
	// For Stripe upgrades AmountDueNow is the local Model B estimate (Stripe
	// finalizes the exact proration on its side), so treat it as approximate.
	Currency         string     `json:"currency,omitempty"`
	AmountDueNow     int64      `json:"amount_due_now,string"`
	NextChargeAmount int64      `json:"next_charge_amount,string"`
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	// OperationID names the durable provider operation behind a Stripe tier
	// change or an NMI upgrade. A "processing" answer (HTTP 202) carries it
	// while the provider outcome is unresolved; the same Idempotency-Key
	// replays the stored result.
	OperationID string `json:"operation_id,omitempty"`
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
)

type TierChangePreviewResponse struct {
	Object           string     `json:"object"` // "tier_change_preview"
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

type CheckoutSessionNextAction struct {
	Type          string                        `json:"type"`
	RedirectToURL *CheckoutSessionRedirectToURL `json:"redirect_to_url,omitempty"`
	// Transactions carries base64-encoded UNSIGNED Solana transactions the
	// subscriber's wallet must sign + send, in order, for type
	// "solana_sign_transactions" (recurring subscribe, #261). After sending, the
	// frontend calls confirm with the resulting signature; if the session is still
	// requires_action it signs the next returned transaction and confirms again.
	Transactions []string `json:"transactions,omitempty"`
}

type CheckoutSessionRedirectToURL struct {
	URL string `json:"url,omitempty"`
}

type CheckoutSessionPaymentResponse struct {
	Rail           string `json:"rail"`
	Reference      string `json:"reference,omitempty"`
	TransactionURL string `json:"transaction_url,omitempty"`
	SolanaPayURL   string `json:"solana_pay_url,omitempty"`
	RedirectURL    string `json:"redirect_url,omitempty"`
	TransactionID  string `json:"transaction_id,omitempty"`
}

type ChangeTierRequest struct {
	PriceID PriceID `json:"price_id"`
}
