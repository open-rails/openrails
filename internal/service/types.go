// Package service provides the in-process billing API for embedded hosts.
//
// All types in this file are exported and safe to use by external packages.
// These types do not import from internal/* packages.
package service

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
