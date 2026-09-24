package requestauth

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOnceScopesVerificationToRequestAndOwner(t *testing.T) {
	r := Begin(httptest.NewRequest("GET", "/", nil))
	require.Same(t, r, Begin(r), "nested mounts share one cache")
	ctx, other := r.Context(), Begin(httptest.NewRequest("GET", "/", nil)).Context()
	ownerA, ownerB := new(int), new(int)
	var calls atomic.Int32
	denied := errors.New("credential denied")
	verify := func() (any, error) { calls.Add(1); return nil, denied }

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			v, err := Once(ctx, ownerA, verify)
			if v != nil || !errors.Is(err, denied) {
				t.Errorf("cached nil-interface failure = %v, %v", v, err)
			}
		})
	}
	wg.Wait()
	require.EqualValues(t, 1, calls.Load(), "concurrent callers share one verification")
	_, _ = Once(ctx, ownerB, verify)
	_, _ = Once(other, ownerA, verify)
	require.EqualValues(t, 3, calls.Load(), "distinct owners and requests never share a result")
	_, _ = Once(context.Background(), ownerA, verify)
	_, _ = Once(context.Background(), ownerA, verify)
	require.EqualValues(t, 5, calls.Load(), "without Begin nothing is cached")
}

func TestHostPrincipalIsContextOnly(t *testing.T) {
	_, ok := HostPrincipalFromContext(context.Background())
	require.False(t, ok)
	_, ok = HostPrincipalFromContext(WithHostPrincipal(context.Background(), nil))
	require.False(t, ok, "a nil principal is not a principal")
	p := &HostPrincipal{Subject: "host"}
	got, ok := HostPrincipalFromContext(WithHostPrincipal(context.Background(), p))
	require.True(t, ok)
	require.Same(t, p, got)
}
