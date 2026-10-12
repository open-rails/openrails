package requestauth

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

func TestHostPrincipalIsContextOnly(t *testing.T) {
	_, ok := HostPrincipalFromContext(context.Background())
	require.False(t, ok)
	_, ok = HostPrincipalFromContext(WithHostPrincipal(context.Background(), nil))
	require.False(t, ok, "a nil principal is not a principal")
	p := &HostPrincipal{MerchantID: billing.MerchantID(uuid.New())}
	got, ok := HostPrincipalFromContext(WithHostPrincipal(context.Background(), p))
	require.True(t, ok)
	require.Same(t, p, got)
}
