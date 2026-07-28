package api

import (
	"time"
)

// ProductObject represents a product resource
type ProductObject struct {
	ID               string                           `json:"id"`
	Object           string                           `json:"object"` // Always "product"
	Key              string                           `json:"key"`
	Name             string                           `json:"name"`
	Description      string                           `json:"description"`
	EntitlementsSpec map[string]*int                  `json:"entitlements_spec,omitempty"`
	CreditsSpec      map[string]CreditGrantSpecObject `json:"credits_spec,omitempty"`
	TierGroup        *string                          `json:"tier_group,omitempty"`
	TierRank         int                              `json:"tier_rank"`
	Active           bool                             `json:"active"`
	Metadata         map[string]string                `json:"metadata,omitempty"`
	Created          int64                            `json:"created"`
	Updated          int64                            `json:"updated"`
	Prices           []PriceObject                    `json:"prices,omitempty"`
}

// CreditGrantSpecObject describes a product-bundled credit grant. A null/absent
// expiry_hours means the granted balance never expires (#857).
type CreditGrantSpecObject struct {
	Unit        string `json:"unit,omitempty"`
	Amount      int64  `json:"amount"`
	ExpiryHours *int   `json:"expiry_hours,omitempty"`
	Cadence     string `json:"cadence,omitempty"`
}

// PriceObject represents a price resource
type PriceObject struct {
	ID string `json:"id"`
	// Key (#774) is the durable, merchant-unique movable-pointer handle for
	// this price's substance-version chain — usable anywhere `id` is accepted.
	Key        string         `json:"key,omitempty"`
	Object     string         `json:"object"`      // Always "price"
	UnitAmount int64          `json:"unit_amount"` // In the currency's smallest unit
	Currency   string         `json:"currency"`
	Type       string         `json:"type,omitempty"`      // one_time or recurring
	Recurring  *RecurringInfo `json:"recurring,omitempty"` // null for one-time purchases
	Product    string         `json:"product"`             // Product ID
	Active     bool           `json:"active"`
	// Providers lists the payment rails this price can be paid through
	// (e.g. ["stripe","ccbill"]), sorted. Lets clients render per-provider
	// checkout actions and choose which rail to send at checkout time.
	Providers []string          `json:"providers,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Created   int64             `json:"created"`
}

// RecurringInfo describes the billing interval for recurring prices
type RecurringInfo struct {
	Interval string `json:"interval"` // hours, e.g. "720h", "8760h"
}

// PaymentObject represents a payment resource
type PaymentObject struct {
	ID             string  `json:"id"`
	Object         string  `json:"object"`           // "charge" for Stripe-style responses
	Status         string  `json:"status,omitempty"` // succeeded, pending, failed, refunded, partially_refunded
	Amount         int64   `json:"amount"`           // Amount in cents (positive for payments, negative for refunds)
	AmountRefunded int64   `json:"amount_refunded"`
	Currency       string  `json:"currency"`
	User           string  `json:"user"`                   // User ID with usr_ prefix
	Subscription   *string `json:"subscription,omitempty"` // Subscription ID if linked
	Rail           string  `json:"rail"`                   // nmi, ccbill, solana
	TransactionID  string  `json:"transaction_id"`         // Rail's transaction identifier
	Refunded       bool    `json:"refunded"`               // True if fully refunded
	Captured       bool    `json:"captured,omitempty"`     // Always true for immediate captures
	// FailureCode is the raw rail decline code; FailureReason the normalized category.
	FailureCode   *string             `json:"failure_code,omitempty"`
	FailureReason *string             `json:"failure_reason,omitempty"`
	Refunds       *PaymentRefundsList `json:"refunds,omitempty"` // List of refunds (for single payment view)
	Created       int64               `json:"created"`           // Unix epoch seconds
	Price         *PriceObject        `json:"price,omitempty"`   // Expanded price object
}

// PaymentRefundsList contains refund entries for a payment
type PaymentRefundsList struct {
	Object string          `json:"object"` // Always "list"
	Data   []PaymentObject `json:"data"`
}

// List is a Stripe-style list response with offset/limit pagination. It mirrors
// the Gin response package shape without importing Gin, keeping pkg/embedded
// usable from pure net/http callers (#285).
type List[T any] struct {
	Object  string `json:"object"`   // Always "list"
	Data    []T    `json:"data"`     // The items
	Total   int64  `json:"total"`    // Total count across all pages
	Limit   int    `json:"limit"`    // Max items requested
	Offset  int    `json:"offset"`   // Items skipped
	HasMore bool   `json:"has_more"` // More items available
}

// NewList creates a List response with has_more calculated automatically.
func NewList[T any](data []T, total int64, limit, offset int) List[T] {
	if data == nil {
		data = []T{}
	}
	return List[T]{
		Object:  "list",
		Data:    data,
		Total:   total,
		Limit:   limit,
		Offset:  offset,
		HasMore: int64(offset+len(data)) < total,
	}
}

// Helper to convert time.Time to unix epoch
func ToUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
