package catalogpolicy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/stretchr/testify/require"
)

func TestCatalogWritesDeniedUnlessMountedOrOwner(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, Check(ctx, nil), ErrUpdatesDisabled, "a missing policy never enables writes")
	exposure := &Exposure{}
	require.ErrorIs(t, Check(ctx, exposure), ErrUpdatesDisabled, "undecided is closed")
	require.NoError(t, Check(OperatorContext(ctx), exposure))
	host := requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{Subject: "host"})
	require.NoError(t, Check(host, exposure), "the process owner writes its own catalog")

	require.NoError(t, exposure.Decide(false))
	require.ErrorIs(t, Check(ctx, exposure), ErrUpdatesDisabled)
	require.NoError(t, exposure.Decide(false), "a second agreeing mount")
	require.ErrorContains(t, exposure.Decide(true), "every mount must agree")
	require.ErrorIs(t, Check(ctx, exposure), ErrUpdatesDisabled, "a refused mount changes nothing")

	open := &Exposure{}
	require.NoError(t, open.Decide(true))
	require.NoError(t, Check(ctx, open))
	require.ErrorContains(t, open.Decide(false), "every mount must agree")

	child, cancel := context.WithCancel(OperatorContext(ctx))
	defer cancel()
	require.NoError(t, Check(child, nil), "derived contexts keep operator authority")
	type lookalike string
	require.ErrorIs(t, Check(context.WithValue(ctx, lookalike("operator"), true), nil), ErrUpdatesDisabled, "only the private key grants authority")
}
