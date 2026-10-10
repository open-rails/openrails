package billing

import (
	"time"
)

// Customer is one merchant-scoped billing record. Its ID is the host's stable
// subject UUID; the same UUID under another merchant is a different customer.
// Contact is who the customer is, from the merchant's directory; null when the
// directory holds no contact for it. The rest summarizes its billing: its
// settings, and per currency its balance and the card that pays its invoices.
// Every growing list (subscriptions, payments, cards, entitlements, product
// access) is its own paginated route.
type Customer struct {
	ID         CustomerID       `json:"id"`
	Contact    *CustomerContact `json:"contact"`
	CreatedAt  time.Time        `json:"created_at"`
	LastSeenAt time.Time        `json:"last_seen_at"`
	Settings   CustomerSettings `json:"settings"`
	// Balances are the currencies the customer holds money, settings or debt in.
	Balances []Balance `json:"balances"`
	// CollectionPaymentMethods are the cards that pay each currency's
	// invoices; a currency without one is absent.
	CollectionPaymentMethods []CollectionPaymentMethod `json:"collection_payment_methods"`
}

// CustomerContact is how to reach a customer, as the merchant's directory
// holds it: read live from the host's directory when OpenRails is embedded
// beside it (Deps.UserInfo), else the copy its SCIM provisioning and verified
// access tokens keep. Active and SyncedAt describe that copy and are null for
// a live read.
type CustomerContact struct {
	Email    *string `json:"email"`
	Name     *string `json:"name"`
	Username *string `json:"username"`
	// Active is the directory's SCIM active flag, shown and never enforced:
	// the host's auth decides who signs in.
	Active *bool `json:"active"`
	// SyncedAt is when the copy last changed.
	SyncedAt *time.Time `json:"synced_at"`
}

// CustomerAccount is the signed-in customer's own summary (GET /v1/me): per
// currency its balance and the card that pays its invoices, and how many of
// its notices are unread.
type CustomerAccount struct {
	ID                       CustomerID                `json:"id"`
	Balances                 []Balance                 `json:"balances"`
	CollectionPaymentMethods []CollectionPaymentMethod `json:"collection_payment_methods"`
	UnreadNotifications      int64                     `json:"unread_notifications"`
}

// CustomerListParams lists customers, newest first. Search instead lists the
// customers whose email, username or name contains it, or whose id it is: at
// most limit, newest first, in one page. IDs instead reads 1 to
// MaxBatchItems named customers in one page; unknown ones are absent.
type CustomerListParams struct {
	Search string       `form:"search"`
	IDs    []CustomerID `form:"-"`
	PageRequest
}
