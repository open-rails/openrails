package billing

import "time"

// Page is the common bounded list envelope. Empty lists contain data: [].
type Page[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	Total   int64  `json:"total"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
	HasMore bool   `json:"has_more"`
}

type PageOptions struct{ Limit, Offset int }

// SubscriptionStatus is a subscription's lifecycle state: whether it will
// rebill.
type SubscriptionStatus string

const (
	SubscriptionPending        SubscriptionStatus = "pending"
	SubscriptionActive         SubscriptionStatus = "active"
	SubscriptionPastDue        SubscriptionStatus = "past_due"
	SubscriptionAwaitingMethod SubscriptionStatus = "awaiting_method"
	SubscriptionCanceled       SubscriptionStatus = "canceled"
	// SubscriptionUnverified: the provider must say whether it still bills.
	SubscriptionUnverified SubscriptionStatus = "unverified"
)

// SubscriptionListParams filters one page of the merchant's subscriptions,
// newest first.
type SubscriptionListParams struct {
	PageRequest
	CustomerID CustomerID
	Status     SubscriptionStatus
	Rail       string
	PriceID    PriceID
}

// Subscription exposes lifecycle and recovery state without making provider
// credentials or mutable storage models part of the client contract. Ids are
// the typed family of ids.go. The
// merchant routes and the customer's own /v1/me/subscriptions routes serve
// this one shape; the self routes additionally fill ScheduledPrice,
// ScheduledProduct, CancelPortalURL and Access.
type Subscription struct {
	// CollectionPolicy is read-only scheduling/recovery ownership.
	CollectionPolicy    string           `json:"collection_policy"`
	Recovery            *PaymentRecovery `json:"recovery,omitempty"`
	LastRetryAt         *time.Time       `json:"last_retry_at"`
	RetryAttempts       *int             `json:"retry_attempts"`
	NextRetryAt         *time.Time       `json:"next_retry_at"`
	GraceEndsAt         *time.Time       `json:"grace_ends_at"`
	DeletionScheduledAt *time.Time       `json:"deletion_scheduled_at,omitempty"`
	// Payments is the subscription's recovery history: the same Payment shape
	// GET /v1/merchant/payments serves.
	Payments              []Payment          `json:"payments,omitempty"`
	ID                    SubscriptionID     `json:"id"`
	CustomerID            CustomerID         `json:"customer_id"`
	ProductID             ProductID          `json:"product_id"`
	PriceID               PriceID            `json:"price_id"`
	PSPID                 PSPID              `json:"psp_id"`
	Rail                  string             `json:"rail"`
	RailSubscriptionID    string             `json:"rail_subscription_id"`
	Status                SubscriptionStatus `json:"status"`
	ScheduledPriceID      *PriceID           `json:"scheduled_price_id,omitempty"`
	PaymentMethodID       *PaymentMethodID   `json:"payment_method_id"`
	StartedAt             time.Time          `json:"started_at"`
	EndedAt               *time.Time         `json:"ended_at"`
	CurrentPeriodStartsAt *time.Time         `json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time         `json:"current_period_ends_at"`
	CanceledAt            *time.Time         `json:"canceled_at"`
	CancelType            *string            `json:"cancel_type"`
	CancelFeedback        *string            `json:"cancel_feedback"`
	Resumable             bool               `json:"resumable"`
	CancelScheduled       bool               `json:"cancel_scheduled"`
	CancelMode            string             `json:"cancel_mode"`
	Price                 *Price             `json:"price,omitempty"`
	Product               *ProductSummary    `json:"product,omitempty"`
	ScheduledPrice        *Price             `json:"scheduled_price,omitempty"`
	ScheduledProduct      *ProductSummary    `json:"scheduled_product,omitempty"`
	// Card is display data for the card behind PaymentMethodID, when it is one.
	Card *CardDetails `json:"card,omitempty"`
	// CancelPortalURL is where the customer cancels when CancelMode is
	// external_portal (rails that keep cancellation on their own site).
	CancelPortalURL *string `json:"cancel_portal_url,omitempty"`
	// Access summarizes the premium access this subscription grants; the self
	// routes fill it.
	Access *SubscriptionAccess `json:"access,omitempty"`
	// NextAction is set only on the answer to an action whose rail needs the
	// customer's own step before it takes effect (a Solana cancel is signed by
	// the customer's wallet). The subscription is then unchanged; complete the
	// step and repeat the request with its result.
	NextAction *NextAction `json:"next_action"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// SubscriptionAccess is how a customer currently holds premium access: Kind
// is subscription (the subscription itself) or entitlement (a standing
// entitlement window, e.g. a one-off purchase or an admin grant). SourceID is
// the source's own wire id (see SourceRef).
type SubscriptionAccess struct {
	Kind           string         `json:"kind"`
	Entitlement    string         `json:"entitlement"`
	SourceType     string         `json:"source_type,omitempty"`
	SourceID       string         `json:"source_id,omitempty"`
	SubscriptionID SubscriptionID `json:"subscription_id,omitzero"`
	Rail           string         `json:"rail,omitempty"`
	StartAt        time.Time      `json:"start_at"`
	EndAt          *time.Time     `json:"end_at,omitempty"`
}

// ProductSummary names the product a subscription or payment is for. It is
// not the catalog Product: that one carries the product's current prices and
// entitlement spec, which are not about this subscription or payment, whose
// own price is beside it.
type ProductSummary struct {
	ID          ProductID `json:"id"`
	Key         string    `json:"key"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	TierGroup   *string   `json:"tier_group"`
	TierRank    int       `json:"tier_rank"`
	Archived    bool      `json:"archived"`
}

// CancelSubscriptionParams is the merchant's cancel: at period end (access
// kept to the paid period end) or, with RevokeAccess, immediately. Both stop
// provider billing.
type CancelSubscriptionParams struct {
	Reason       string `json:"reason"`
	RevokeAccess bool   `json:"revoke_access,omitempty"`
	// AccountDeletion marks the host's irrevocable account-deletion cancel.
	// While destructive provider actions are disarmed an ordinary cancel of a
	// provider-billed subscription is refused (CodeProviderCancelHeld); an
	// account deletion is canceled locally and its provider schedule delete
	// waits for the operator's arming.
	AccountDeletion bool `json:"account_deletion,omitempty"`
}

type UpdateSubscriptionPaymentMethodParams struct {
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}

// CodeCustomerActionRequired: only the customer can take this action, through
// their own step (a wallet signature, the provider's hosted page, an
// authenticated payment). Nothing changed.
const CodeCustomerActionRequired = "customer_action_required"

// CodePaymentMethodPSPMismatch: the saved method the request named was vaulted
// by a different provider account than the one that owns the subscription
// (type invalid_request_error, 409). Provider vault references are
// account-scoped, so nothing was sent to the provider; collect the card again
// on the subscription's active provider account.
const CodePaymentMethodPSPMismatch = "payment_method_psp_mismatch"

var ErrPaymentMethodPSPMismatch error = newCodedError(CodePaymentMethodPSPMismatch, ErrConflict)
