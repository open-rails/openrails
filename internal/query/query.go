// Package query is the shared list-query shape for paginated endpoints: bind
// page/page_size (or limit/offset) off the request, carry an endpoint-specific
// Filters payload, and hand the resolved limit/offset to the data layer.
package query

// QueryOptions is one list request: pagination plus the endpoint's own filters.
type QueryOptions[T any] struct {
	Page       int    `form:"page" json:"page"`
	SortBy     string `form:"sort_by" json:"sort_by"`
	PageSize   int    `form:"page_size" json:"page_size"`
	Limit      int    `form:"limit" json:"limit"`
	Offset     int    `form:"offset" json:"offset"`
	TotalItems int64  `form:"total_items" json:"total_items"`
	All        bool   `form:"all" json:"all"`
	Filters    T      `form:",inline"`
}

// Any erases the Filters type parameter, for layers that only need pagination.
func (q QueryOptions[T]) Any() QueryOptions[any] {
	return QueryOptions[any]{
		Page:       q.Page,
		SortBy:     q.SortBy,
		PageSize:   q.PageSize,
		Limit:      q.Limit,
		Offset:     q.Offset,
		All:        q.All,
		TotalItems: q.TotalItems,
	}
}
