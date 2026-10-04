// Package service provides the in-process billing API for embedded hosts.
//
// All types in this file are exported and safe to use by external packages.
// These types do not import from internal/* packages.
package service

import (
	"github.com/open-rails/openrails/billing"
)

// -------------------------------- Pagination --------------------------------

// PaginationOptions specifies limit/offset pagination parameters.
type PaginationOptions struct {
	Limit  int
	Offset int
}

// PaginatedResult wraps a paginated response with total count.
type PaginatedResult[T any] struct {
	Data       []T
	TotalItems int64
	Limit      int
	Offset     int
}

// -------------------------------- Billing Status --------------------------------

// EffectiveTier is the single winning tier for a user within one tier group
// (or#912). Entitlement and ProductKey are IMMUTABLE identifiers — hosts key
// policy documents and token claims on Entitlement; DisplayName is mutable
// and for display only.
type EffectiveTier = billing.EffectiveTier
