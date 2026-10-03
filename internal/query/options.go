package query

// GetLimit returns the limit for pagination
func (o *QueryOptions[T]) GetLimit() int {
	if o.Limit > 0 {
		return o.Limit
	}
	return o.PageSize
}

// GetOffset returns the offset for pagination
func (o *QueryOptions[T]) GetOffset() int {
	if o.Offset > 0 {
		return o.Offset
	}
	return (o.Page - 1) * o.PageSize
}

// SetTotal sets the total count in the QueryOptions
// and returns the QueryOptions for chaining
func (o *QueryOptions[T]) SetTotal(total int64) *QueryOptions[T] {
	o.TotalItems = total
	return o
}

// GetTotal returns the total items count
func (o *QueryOptions[T]) GetTotal() int64 {
	return o.TotalItems
}

// TotalPages calculates and returns the total number of pages
func (o *QueryOptions[T]) TotalPages() int {
	if o.All || o.PageSize <= 0 {
		return 1
	}
	return int((o.TotalItems + int64(o.PageSize) - 1) / int64(o.PageSize))
}
