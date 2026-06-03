package query

import (
	"math"
	"net/url"
)

type Response struct {
	Items      any   `json:"items" binding:"required"`
	Page       int   `json:"page" binding:"required"`
	PageSize   int   `json:"page_size" binding:"required"`
	TotalItems int64 `json:"total_items" binding:"required"`
	TotalPages int   `json:"total_pages" binding:"required"`
}

type QueryOptions[T any] struct {
	Page        int    `form:"page" json:"page"`
	SearchQuery string `form:"search" json:"search"`
	SortBy      string `form:"sort_by" json:"sort_by"`
	OrderBy     string `form:"order_by" json:"order_by"`
	PageSize    int    `form:"page_size" json:"page_size"`
	Limit       int    `form:"limit" json:"limit"`
	Offset      int    `form:"offset" json:"offset"`
	TotalItems  int64  `form:"total_items" json:"total_items"`
	All         bool   `form:"all" json:"all"`
	Filters     T      `form:",inline"`
}

type QueryParser interface {
	GetQuery() url.Values
	Query(key string) string
	DefaultQuery(key, defaultValue string) string
}

const (
	defaultPage     = 1
	defaultPageSize = 10
	maxPageSize     = 100
)

func (q QueryOptions[T]) Any() QueryOptions[any] {
	return QueryOptions[any]{
		Page:        q.Page,
		SearchQuery: q.SearchQuery,
		SortBy:      q.SortBy,
		OrderBy:     q.OrderBy,
		PageSize:    q.PageSize,
		Limit:       q.Limit,
		Offset:      q.Offset,
		All:         q.All,
		TotalItems:  q.TotalItems,
	}
}

// func isFilterParam(param string) bool {
// 	nonFilterParams := map[string]bool{
// 		"sort":      true,
// 		"order":     true,
// 		"search":    true,
// 		"page":      true,
// 		"page_size": true,
// 	}

// 	return !nonFilterParams[param]
// }

func (q QueryOptions[T]) PaginatedResponse(items any, totalItems int64) Response {
	// If "all" is requested, return all items without pagination metadata
	if q.All {
		return Response{
			Items:      items,
			Page:       1,
			PageSize:   int(totalItems),
			TotalItems: totalItems,
			TotalPages: 1,
		}
	}

	totalPages := int(math.Ceil(float64(totalItems) / float64(q.PageSize)))

	if totalItems == 0 {
		items = []any{}
	}

	return Response{
		Items:      items,
		Page:       q.Page,
		PageSize:   q.PageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
	}
}
