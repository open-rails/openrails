package openrails

import (
	"time"

	"github.com/google/uuid"
)

// Customer payment recovery (#809): the payer pays an open invoice now or
// retries a past-due subscription now. Both run the engine's own durable
// collection machinery — a new immutable attempt under one idempotent
// operation — so history is never rewritten and a lost provider answer stays
// visibly unresolved instead of being resent.
//
// Only rails where OpenRails both drives dunning and charges saved methods
// (today: NMI) are accepted; provider-managed rails are refused before any
// provider traffic with CodePaymentRecoveryRailUnsupported.

// PaymentRecovery is the recovery state the customer routes attach to an
// invoice or subscription: whether the engine would accept pay-now/retry-now
// right now, why not, and what it will do on its own.
type PaymentRecovery struct {
	// Retryable reports that pay-now/retry-now would be accepted right now.
	Retryable bool `json:"retryable"`
	// BlockedReason names why Retryable is false (RecoveryBlocked* values).
	BlockedReason string `json:"blocked_reason,omitempty"`
	// NextAttemptAt is the engine's own next scheduled attempt, if any.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	// AttemptCount is every recorded attempt, whatever its outcome.
	AttemptCount int `json:"attempt_count"`
	// FailureCategory is the normalized reason of the newest failed attempt
	// (insufficient_funds, expired_card, generic_decline, ...).
	FailureCategory string `json:"failure_category,omitempty"`
	// LastFailureCode is the provider's verbatim code of that attempt.
	LastFailureCode string     `json:"last_failure_code,omitempty"`
	LastFailedAt    *time.Time `json:"last_failed_at,omitempty"`
	// CompatiblePaymentMethodIDs are the payer's saved methods a recovery
	// charge may use. For a subscription, retry-now charges its current method;
	// the others are what PUT /payment-method accepts first.
	CompatiblePaymentMethodIDs []PaymentMethodID `json:"compatible_payment_method_ids"`
	// Operation is the live durable operation while one is unresolved.
	Operation *PaymentOperation `json:"operation,omitempty"`
}

// PaymentOperation names one durable collection operation and its state.
// Status is pending, in_flight, failed_retryable, unknown_needs_verify
// (unresolved: no provider answer yet), or succeeded, failed_terminal,
// superseded, expired.
type PaymentOperation struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

// Unresolved reports that the provider outcome is not yet known.
func (o PaymentOperation) Unresolved() bool {
	switch o.Status {
	case "pending", "in_flight", "unknown_needs_verify", "failed_retryable":
		return true
	}
	return false
}

// PaymentRecovery.BlockedReason values.
const (
	RecoveryBlockedNotDue          = "not_due"
	RecoveryBlockedUncollectible   = "uncollectible"
	RecoveryBlockedInProgress      = "in_progress"
	RecoveryBlockedOutcomeUnknown  = "outcome_unknown"
	RecoveryBlockedRailUnsupported = "rail_unsupported"
	RecoveryBlockedNoPaymentMethod = "no_compatible_payment_method"
	// RecoveryBlockedPSPMismatch: the subscription's saved method was vaulted
	// by another provider account (#657); collect the card again on the
	// subscription's account.
	RecoveryBlockedPSPMismatch = "payment_method_psp_mismatch"
	// RecoveryBlockedWindowExpired: the missed renewal is older than the
	// dunning window (#839); nothing charges it any more.
	RecoveryBlockedWindowExpired = "dunning_window_expired"
)

// PayInvoiceNowRequest charges one open or past-due invoice through a saved
// method the payer owns. IdempotencyKey is the canonical Idempotency-Key: the
// same key replays the same attempt without a second charge. CustomerID is the
// payer a host acts for over the Client; the self routes take it from the
// authenticated principal.
type PayInvoiceNowRequest struct {
	InvoiceID       uuid.UUID       `json:"-"`
	CustomerID      CustomerID      `json:"-"`
	IdempotencyKey  string          `json:"-"`
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

// InvoicePayNowResult is the pay-now answer: HTTP 200 once the attempt is
// terminal, 202 while Operation is unresolved (poll GET /invoices/{id} and
// its /payments). A decline is not a result; it is the 402 card_declined
// refusal whose metadata carries the attempt.
type InvoicePayNowResult struct {
	Invoice   InvoiceDTO               `json:"invoice"`
	Attempt   InvoicePaymentAttemptDTO `json:"attempt"`
	Operation PaymentOperation         `json:"operation"`
	Replayed  bool                     `json:"replayed"`
}

// RetrySubscriptionNowRequest rebills one past-due subscription now through
// its current saved method. PaymentMethodID, when given, must be that method.
type RetrySubscriptionNowRequest struct {
	SubscriptionID  SubscriptionID   `json:"-"`
	CustomerID      CustomerID       `json:"-"`
	IdempotencyKey  string           `json:"-"`
	PaymentMethodID *PaymentMethodID `json:"payment_method_id,omitempty"`
}

// SubscriptionRetryNowResult is the retry-now answer: 200 with the renewed
// subscription and its confirmed Payment, or 202 while Operation is
// unresolved (poll GET /subscriptions/{id}). A decline is the 402
// card_declined refusal.
type SubscriptionRetryNowResult struct {
	Subscription Subscription     `json:"subscription"`
	Payment      *Payment         `json:"payment,omitempty"`
	Operation    PaymentOperation `json:"operation"`
	Replayed     bool             `json:"replayed"`
}

// Payment recovery refusal codes.
const (
	// CodePaymentRecoveryRailUnsupported (409): the rail drives its own
	// dunning or cannot charge a saved method, so a customer retry could
	// double charge. Nothing reached the provider.
	CodePaymentRecoveryRailUnsupported = "payment_recovery_rail_unsupported"
	// CodeSubscriptionNotRetryable (409): the subscription is not past due, has
	// no rebillable period, or its saved method cannot be charged.
	CodeSubscriptionNotRetryable = "subscription_not_retryable"
	// CodeSubscriptionRetryInProgress (409): a rebill for this period is
	// executing (the customer's or the dunning schedule's).
	CodeSubscriptionRetryInProgress = "subscription_retry_in_progress"
	// CodeSubscriptionRetryOutcomeUnknown (409): a submitted rebill has no
	// provider answer yet; nothing is resent until it resolves.
	CodeSubscriptionRetryOutcomeUnknown = "subscription_retry_outcome_unknown"
	// CodeSubscriptionRetryIdempotencyConflict (409): the Idempotency-Key
	// already names a different retry-now request (another subscription or
	// payment method). Nothing is charged and nothing is replayed.
	CodeSubscriptionRetryIdempotencyConflict = "subscription_retry_idempotency_conflict"
)

var (
	ErrPaymentRecoveryRailUnsupported       error = newCodedError(CodePaymentRecoveryRailUnsupported, ErrConflict)
	ErrSubscriptionNotRetryable             error = newCodedError(CodeSubscriptionNotRetryable, ErrConflict)
	ErrSubscriptionRetryInProgress          error = newCodedError(CodeSubscriptionRetryInProgress, ErrConflict)
	ErrSubscriptionRetryOutcomeUnknown      error = newCodedError(CodeSubscriptionRetryOutcomeUnknown, ErrConflict)
	ErrSubscriptionRetryIdempotencyConflict error = newCodedError(CodeSubscriptionRetryIdempotencyConflict, ErrConflict)
	ErrInvoiceRetryIdempotencyConflict      error = newCodedError(CodeInvoiceRetryIdempotencyConflict, ErrConflict)
	ErrInvoiceNotRetryable                  error = newCodedError(CodeInvoiceNotRetryable, ErrConflict)
	ErrInvoiceRetryInProgress               error = newCodedError(CodeInvoiceRetryInProgress, ErrConflict)
	ErrInvoiceRetryOutcomeUnknown           error = newCodedError(CodeInvoiceRetryOutcomeUnknown, ErrConflict)
	ErrCollectionPaymentMethodInvalid       error = newCodedError(CodeCollectionPaymentMethodInvalid, ErrInvalid)
)
