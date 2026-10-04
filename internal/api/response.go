package api

import (
	"github.com/open-rails/openrails/billing"
)

// List is a Stripe-style list response with offset/limit pagination. It mirrors
// the Gin response package shape without importing Gin, keeping pkg/embedded
// usable from pure net/http callers (#285).
type List[T any] = billing.Page[T]

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
