package api

import (
	"time"

	"github.com/open-rails/openrails"
)

// ProductObject represents a product resource
type ProductObject struct {
	ID               openrails.ProductID `json:"id"`
	Object           string              `json:"object"` // Always "product"
	Key              string              `json:"key"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	EntitlementsSpec map[string]*int     `json:"entitlements_spec,omitempty"`
	TierGroup        *string             `json:"tier_group,omitempty"`
	TierRank         int                 `json:"tier_rank"`
	Active           bool                `json:"active"`
	Metadata         map[string]string   `json:"metadata,omitempty"`
	Created          int64               `json:"created"`
	Updated          int64               `json:"updated"`
	Prices           []PriceObject       `json:"prices,omitempty"`
}

// These aliases share the public Client wire types.
type (
	PriceObject        = openrails.PublicPrice
	RecurringInfo      = openrails.PriceRecurrence
	PaymentObject      = openrails.Payment
	PaymentRefundsList = openrails.PaymentList
)

// List is a Stripe-style list response with offset/limit pagination. It mirrors
// the Gin response package shape without importing Gin, keeping pkg/embedded
// usable from pure net/http callers (#285).
type List[T any] = openrails.Page[T]

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
