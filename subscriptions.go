package openrails

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

type SubscriptionFilter struct {
	PageOptions
	CustomerID CustomerID
	Status     string
	Rail       string
}

// Subscription exposes lifecycle and recovery state without making provider
// credentials or mutable storage models part of the client contract. Ids are
// the typed family of ids.go; PSPID is the provider account's plain UUID. The
// merchant routes and the customer's own /v1/me/subscriptions routes serve
// this one shape; the self routes additionally fill ScheduledPrice,
// ScheduledProduct, CancelPortalURL and Access.
type Subscription struct {
	LastRetryAt         *time.Time `json:"last_retry_at"`
	RetryAttempts       *int       `json:"retry_attempts"`
	NextRetryAt         *time.Time `json:"next_retry_at"`
	GraceEndsAt         *time.Time `json:"grace_ends_at"`
	DeletionScheduledAt *time.Time `json:"deletion_scheduled_at,omitempty"`
	// Payments is the subscription's recovery history: the same Payment shape
	// GET /v1/merchant/payments serves.
	Payments              []Payment            `json:"payments,omitempty"`
	ID                    SubscriptionID       `json:"id"`
	CustomerID            CustomerID           `json:"customer_id"`
	ProductID             ProductID            `json:"product_id"`
	PriceID               PriceID              `json:"price_id"`
	PSPID                 string               `json:"psp_id"`
	Rail                  string               `json:"rail"`
	RailSubscriptionID    string               `json:"rail_subscription_id"`
	Status                string               `json:"status"`
	ScheduledPriceID      *PriceID             `json:"scheduled_price_id,omitempty"`
	PaymentMethodID       *PaymentMethodID     `json:"payment_method_id"`
	StartedAt             time.Time            `json:"started_at"`
	EndedAt               *time.Time           `json:"ended_at"`
	CurrentPeriodStartsAt *time.Time           `json:"current_period_starts_at"`
	CurrentPeriodEndsAt   *time.Time           `json:"current_period_ends_at"`
	CancelledAt           *time.Time           `json:"cancelled_at"`
	CancelType            *string              `json:"cancel_type"`
	CancelFeedback        *string              `json:"cancel_feedback"`
	Resumable             bool                 `json:"resumable"`
	CancelScheduled       bool                 `json:"cancel_scheduled"`
	CancelMode            string               `json:"cancel_mode"`
	Price                 *SubscriptionPrice   `json:"price,omitempty"`
	Product               *SubscriptionProduct `json:"product,omitempty"`
	ScheduledPrice        *SubscriptionPrice   `json:"scheduled_price,omitempty"`
	ScheduledProduct      *SubscriptionProduct `json:"scheduled_product,omitempty"`
	// Card is display data for the card behind PaymentMethodID, when it is one.
	Card *SubscriptionCard `json:"card,omitempty"`
	// CancelPortalURL is where the customer cancels when CancelMode is
	// external_portal (rails that keep cancellation on their own site).
	CancelPortalURL *string `json:"cancel_portal_url,omitempty"`
	// Access summarizes the premium access this subscription grants; the self
	// routes fill it.
	Access    *SubscriptionAccess `json:"access,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// BillingStatus is GET /v1/me/status: the customer's active subscription (the
// shared Subscription shape) or the standing access they hold without one,
// plus their entitlement windows.
type BillingStatus struct {
	HasActiveSubscription bool                `json:"has_active_subscription"`
	Subscription          *Subscription       `json:"subscription,omitempty"`
	Access                *SubscriptionAccess `json:"access,omitempty"`
	NextRenewalAt         *time.Time          `json:"next_renewal_at,omitempty"`
	Entitlements          []EntitlementRecord `json:"entitlements,omitempty"`
}

// SubscriptionCard is the stored card's display data: no PAN, no token.
type SubscriptionCard struct {
	Brand    string `json:"brand,omitempty"`
	Last4    string `json:"last4,omitempty"`
	ExpMonth *int   `json:"exp_month,omitempty"`
	ExpYear  *int   `json:"exp_year,omitempty"`
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

// SubscriptionPrice is the price a subscription is on; UnitAmount is spelled
// unit_amount as on every other price shape.
type SubscriptionPrice struct {
	ID                  PriceID   `json:"id"`
	Key                 string    `json:"key"`
	ProductID           ProductID `json:"product_id"`
	UnitAmount          int64     `json:"unit_amount,string"`
	Currency            string    `json:"currency"`
	AutoRenew           bool      `json:"auto_renew"`
	AccessDurationHours *int      `json:"access_duration_hours"`
	Archived            bool      `json:"archived"`
}

type SubscriptionProduct struct {
	ID          ProductID `json:"id"`
	Key         string    `json:"key"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	TierGroup   *string   `json:"tier_group,omitempty"`
	TierRank    int       `json:"tier_rank"`
	Archived    bool      `json:"archived"`
}

type CancelSubscriptionRequest struct {
	Reason       string `json:"reason"`
	RevokeAccess bool   `json:"revoke_access,omitempty"`
}

type UpdateSubscriptionPaymentMethodRequest struct {
	PaymentMethodID PaymentMethodID `json:"payment_method_id"`
}
