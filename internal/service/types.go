// Package service is the in-process billing API under the Client and the HTTP
// routes.
package service

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
