//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
)

// TestSDKErgonomics_WithAPIKeyAndVerify runs the #685 SDK ergonomics against the
// REAL standalone server + AuthKit control plane (no stubs): WithAPIKey drives
// the real service-credential middleware end-to-end, and Verify is a true
// authenticated readiness probe — good key OK, bad key ErrUnauthorized,
// unreachable host ErrUnreachable, statically invalid URL a descriptive
// per-call error (the no-error constructor stays I/O-free).
func TestSDKErgonomics_WithAPIKeyAndVerify(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone(money.DefaultCurrency)

	// Good key: resolved by the real API-key gate -> AuthKit core chain.
	good := openrails.NewRemote(standalone.BaseURL,
		openrails.WithAPIKey(standalone.Token),
		openrails.WithTimeout(30*time.Second),
	)
	require.NoError(t, openrails.Verify(ctx, good), "Verify with a real minted API key")
	settings, err := good.GetMerchantSettings(ctx)
	require.NoError(t, err, "authenticated call wired via WithAPIKey")
	require.NotNil(t, settings)

	// Bad key: rejected by the real service-credential middleware -> 401.
	bad := openrails.NewRemote(standalone.BaseURL,
		openrails.WithAPIKey("openrails_st_wrong_token"),
		openrails.WithTimeout(30*time.Second),
	)
	require.ErrorIs(t, openrails.Verify(ctx, bad), openrails.ErrUnauthorized)

	// Unreachable host: the fail-policy sentinel.
	unreachable := openrails.NewRemote("http://127.0.0.1:1",
		openrails.WithAPIKey("whatever"),
		openrails.WithTimeout(2*time.Second),
	)
	require.ErrorIs(t, openrails.Verify(ctx, unreachable), openrails.ErrUnreachable)

	// Statically invalid URL: constructor never errors; every call fails with a
	// descriptive error instead (mintless-tokenFn pattern).
	invalid := openrails.NewRemote("not a url", openrails.WithAPIKey("whatever"))
	err = openrails.Verify(ctx, invalid)
	require.Error(t, err)
	require.Contains(t, err.Error(), "base URL")

	// Empty key: descriptive per-call error naming the option.
	empty := openrails.NewRemote(standalone.BaseURL, openrails.WithAPIKey("  "))
	err = openrails.Verify(ctx, empty)
	require.Error(t, err)
	require.Contains(t, err.Error(), "WithAPIKey")
}
