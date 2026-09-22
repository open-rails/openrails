// Package requestauth holds verification results for one immutable HTTP request.
// It never survives that request or caches a permission decision.
package requestauth

import (
	"context"
	"net/http"
	"sync"
)

type contextKey struct{}
type result struct {
	once  sync.Once
	value any
	err   error
}
type cache struct{ values sync.Map }

// Begin establishes the verification lifetime before HTTP authentication.
func Begin(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(contextKey{}).(*cache); ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), contextKey{}, &cache{}))
}

// Once reuses verification by an explicit, comparable owner pointer. Callers
// must use distinct owners for distinct credential/assurance policies and must
// not alter the request credential, method or URL after verification.
// Concurrent checks share the same completed verification result.
func Once[T any](ctx context.Context, owner any, verify func() (T, error)) (T, error) {
	c, ok := ctx.Value(contextKey{}).(*cache)
	if !ok {
		return verify()
	}
	stored, _ := c.values.LoadOrStore(owner, &result{})
	entry := stored.(*result)
	entry.once.Do(func() { value, err := verify(); entry.value, entry.err = value, err })
	return entry.value.(T), entry.err
}
