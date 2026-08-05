package models

import (
	"time"

	"github.com/google/uuid"
)

type CheckoutSessionMode string

const (
	CheckoutSessionModeOneOff       CheckoutSessionMode = "one_off"
	CheckoutSessionModeSubscription CheckoutSessionMode = "subscription"
	// CheckoutSessionModeSolanaCancel and CheckoutSessionModeSolanaTierChange
	// extend the Solana Pay transaction-request machinery to the recurring
	// subscription lifecycle (#272+). A cancel session carries the target
	// subscription_id in RailState; a tier-change session additionally
	// carries new_price_id. The public Solana Pay endpoint builds the unsigned
	// (or cranker-co-signed) on-chain tx with the Solana Pay reference attached,
	// and the reference poller mirrors the confirmed cancel / tier-change into
	// the DB — the same protocol as a checkout/subscribe session, just a
	// different on-chain action.
	CheckoutSessionModeSolanaCancel     CheckoutSessionMode = "solana_cancel"
	CheckoutSessionModeSolanaTierChange CheckoutSessionMode = "solana_tier_change"
)

type CheckoutSessionStatus string

const (
	CheckoutSessionStatusCreated        CheckoutSessionStatus = "created"
	CheckoutSessionStatusRequiresAction CheckoutSessionStatus = "requires_action"
	CheckoutSessionStatusSucceeded      CheckoutSessionStatus = "succeeded"
	CheckoutSessionStatusFailed         CheckoutSessionStatus = "failed"
	CheckoutSessionStatusExpired        CheckoutSessionStatus = "expired"
	CheckoutSessionStatusCanceled       CheckoutSessionStatus = "canceled"
)

type CheckoutSession struct {
	ID uuid.UUID `json:"id"`
	// CustomerID is the OpenRails payable merchant subject for this row (#317).
	// Join openrails.customers for issuer/subject.
	CustomerID uuid.UUID `json:"customer_id,omitempty"`

	PriceID uuid.UUID           `json:"price_id"`
	Mode    CheckoutSessionMode `json:"mode"`

	Rail   Rail                  `json:"rail"`
	Status CheckoutSessionStatus `json:"status"`

	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`

	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reference *string    `json:"reference,omitempty"`

	TransactionID  *string    `json:"transaction_id,omitempty"`
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"`

	Metadata map[string]string `json:"metadata,omitempty"`

	RailFields map[string]any `json:"rail_fields,omitempty"`
	RailState  map[string]any `json:"rail_state,omitempty"`

	// RoutingReason (or#288) is the decision trace that picked this session's
	// PSP: which policy and rule matched, and what was skipped and why. Written
	// once at creation and never rewritten — support answers "why did this
	// customer get CCBill" from the row, not from a reconstruction.
	RoutingReason *CheckoutRoutingReason `json:"routing_reason,omitempty"`

	// IdempotencyKey is request-scoped only (Redis owns checkout idempotency,
	// #702 dropped the column); never persisted or round-tripped from the DB.
	IdempotencyKey *string `json:"idempotency_key,omitempty"`
	// PspID is the psps row selected for this provider
	// checkout. It prevents provider sessions from being confused across rotated
	// Stripe/NMI/CCBill accounts.
	PspID     uuid.UUID `json:"psp_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Price      *Price  `json:"price,omitempty"`
	LastFour   *string `json:"last_four,omitempty"`
	CardType   *string `json:"card_type,omitempty"`
	ExpiryDate *string `json:"expiry_date,omitempty"`
}

// Checkout routing policy sources (or#288). Recorded on every session so the
// trace names WHO decided, not just what was decided.
const (
	// CheckoutRoutingPolicyExplicit: the request named the PSP. Honoured
	// verbatim — an explicitly named processor is NEVER silently switched,
	// because the browser has already committed to that rail's flow.
	CheckoutRoutingPolicyExplicit = "explicit"
	// CheckoutRoutingPolicyMerchant: a declared merchant routing rule matched.
	CheckoutRoutingPolicyMerchant = "merchant"
	// CheckoutRoutingPolicyDefault: no policy declared (or none matched) — the
	// built-in preference order decided.
	CheckoutRoutingPolicyDefault = "default"
)

// Checkout routing skip classes (or#288): why a candidate ahead of the winner
// was passed over. These are PRE-CHARGE availability facts only. A decline is
// deliberately absent — it is a per-charge outcome, not a routing failure, and
// never re-routes a session.
const (
	// CheckoutRoutingSkipUnknownSelector: not a declared PSP key or rail kind.
	CheckoutRoutingSkipUnknownSelector = "unknown_selector"
	// CheckoutRoutingSkipAmbiguousSelector: a bare rail kind with more than one
	// armed PSP — the #848 selector demands the PSP key.
	CheckoutRoutingSkipAmbiguousSelector = "ambiguous_selector"
	// CheckoutRoutingSkipNotArmed: no active non-archived psps row.
	CheckoutRoutingSkipNotArmed = "not_armed"
	// CheckoutRoutingSkipCredentialsMissing: armed, but the credentials the rail
	// needs to charge are absent or malformed.
	CheckoutRoutingSkipCredentialsMissing = "credentials_missing"
	// CheckoutRoutingSkipLinkMissing: the price carries no usable psp_link for
	// this PSP (absent, or pointing at a different remote object than execution).
	CheckoutRoutingSkipLinkMissing = "link_missing"
	// CheckoutRoutingSkipModeUnsupported: the rail cannot serve this checkout
	// mode for this price (CCBill one-off, Stripe paid intro).
	CheckoutRoutingSkipModeUnsupported = "mode_unsupported"
	// CheckoutRoutingSkipServiceUnavailable: the runtime service backing the
	// rail is not wired in this deployment.
	CheckoutRoutingSkipServiceUnavailable = "service_unavailable"
	// CheckoutRoutingSkipResolveFailed: the selector could not be resolved at
	// all (backend error). Fail closed: skip, never guess.
	CheckoutRoutingSkipResolveFailed = "resolve_failed"
)

// CheckoutRoutingReason is the compact decision trace persisted on
// checkout_sessions.routing_reason.
type CheckoutRoutingReason struct {
	// Policy is who decided: explicit | merchant | default.
	Policy string `json:"policy"`
	// Rule is the matched merchant-rule index (0-based). Merchant policy only.
	Rule *int `json:"rule,omitempty"`
	// Selected is the winning checkout selector (the PSP key), and Rail its kind.
	Selected string `json:"selected"`
	Rail     string `json:"rail"`
	// Fallbacks are the remaining ELIGIBLE selectors, still ranked — what the
	// next session would get if the winner went away.
	Fallbacks []string `json:"fallbacks,omitempty"`
	// Skipped names every candidate passed over ahead of the winner, with its
	// class.
	Skipped []CheckoutRoutingSkip `json:"skipped,omitempty"`
}

// CheckoutRoutingSkip is one passed-over candidate and its class.
type CheckoutRoutingSkip struct {
	Selector string `json:"selector"`
	Reason   string `json:"reason"`
}
