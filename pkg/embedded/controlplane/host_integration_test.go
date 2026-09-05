//go:build integration

package controlplane_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestSetGetMerchantAPIHost proves the openrails-saas #14 seam end to end: a
// host provisions a merchant, sets its api_host through the exported
// SetMerchantAPIHost/GetMerchantAPIHost pair, and the SAME control plane's
// #734 ResolveMerchantByHost resolves it immediately — i.e. this seam writes
// through the identical authority ProvisionMerchant and Host resolution
// already share, not a parallel table.
func TestSetGetMerchantAPIHost(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(t, dsn, "https://hosts.openrails.test")
	e := newHostApp(t, cfg)
	require.NoError(t, embcp.Attach(ctx, e.App(), cfg, nil))

	sfx := strings.ToLower(uuid.NewString()[:8])
	slug := "hostset-" + sfx
	res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: slug})
	require.NoError(t, err)

	// Unset by default.
	host, err := embcp.GetMerchantAPIHost(ctx, e.App(), res.MerchantID)
	require.NoError(t, err)
	require.Empty(t, host)

	apiHost := "api." + slug + ".localtest.me"
	require.NoError(t, embcp.SetMerchantAPIHost(ctx, e.App(), res.MerchantID, apiHost))

	got, err := embcp.GetMerchantAPIHost(ctx, e.App(), res.MerchantID)
	require.NoError(t, err)
	require.Equal(t, apiHost, got)

	// The #734 Host resolver (ResolveMerchantByHost) is the SAME authority:
	// setting the host here must make it resolve immediately, live, with no
	// restart — proving this seam is not a parallel mapping.
	cp := embcp.Get(e.App())
	require.NotNil(t, cp)
	resolved, err := cp.ResolveMerchantByHost(ctx, apiHost)
	require.NoError(t, err)
	require.Equal(t, res.MerchantID, resolved)

	// A different, never-configured host resolves to nothing (fail closed).
	_, err = cp.ResolveMerchantByHost(ctx, "api.never-configured."+sfx+".localtest.me")
	require.Error(t, err)

	// Clearing (empty apiHost) removes the mapping.
	require.NoError(t, embcp.SetMerchantAPIHost(ctx, e.App(), res.MerchantID, ""))
	cleared, err := embcp.GetMerchantAPIHost(ctx, e.App(), res.MerchantID)
	require.NoError(t, err)
	require.Empty(t, cleared)
	_, err = cp.ResolveMerchantByHost(ctx, apiHost)
	require.Error(t, err)

	// A second merchant cannot steal an already-assigned host.
	res2, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "hostset2-" + sfx})
	require.NoError(t, err)
	require.NoError(t, embcp.SetMerchantAPIHost(ctx, e.App(), res2.MerchantID, apiHost))
	res3, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "hostset3-" + sfx})
	require.NoError(t, err)
	err = embcp.SetMerchantAPIHost(ctx, e.App(), res3.MerchantID, apiHost)
	require.Error(t, err)
	require.ErrorIs(t, err, merchants.ErrAPIHostTaken)

	// Without an attached control plane, both calls are a wiring error.
	bare := newHostApp(t, hostedTestConfig(t, dsn, "https://hostset-bare.openrails.test"))
	err = embcp.SetMerchantAPIHost(ctx, bare.App(), merchant.ID{}, apiHost)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no control plane attached")
}
