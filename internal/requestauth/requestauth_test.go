package requestauth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

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
