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
// construction error without I/O.
func TestSDKErgonomics_WithAPIKeyAndVerify(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone(money.DefaultCurrency)

	// Good key: resolved by the real API-key gate -> AuthKit core chain.
	good, goodErr := openrails.NewRemote(standalone.BaseURL,
		openrails.WithAPIKey(standalone.Token),
		openrails.WithTimeout(30*time.Second),
	)
	if goodErr != nil {
		t.Fatal(goodErr)
	}
	require.NoError(t, good.Verify(ctx), "Verify with a real minted API key")
	settings, err := good.GetMerchantSettings(ctx)
	require.NoError(t, err, "authenticated call wired via WithAPIKey")
	require.NotNil(t, settings)

	// Bad key: rejected by the real service-credential middleware -> 401.
	bad, badErr := openrails.NewRemote(standalone.BaseURL,
		openrails.WithAPIKey("openrails_st_wrong_token"),
		openrails.WithTimeout(30*time.Second),
	)
	if badErr != nil {
		t.Fatal(badErr)
	}
	require.ErrorIs(t, bad.Verify(ctx), openrails.ErrUnauthorized)

	// Unreachable host: the fail-policy sentinel.
	unreachable, unreachableErr := openrails.NewRemote("http://127.0.0.1:1",
		openrails.WithAPIKey("whatever"),
		openrails.WithTimeout(2*time.Second),
	)
	if unreachableErr != nil {
		t.Fatal(unreachableErr)
	}
	require.ErrorIs(t, unreachable.Verify(ctx), openrails.ErrUnreachable)

	// Static invalid configuration fails before any operation or I/O.
	invalid, err := openrails.NewRemote("not a url", openrails.WithAPIKey("whatever"))
	require.ErrorContains(t, err, "base URL")
	require.Nil(t, invalid)
	empty, err := openrails.NewRemote(standalone.BaseURL, openrails.WithAPIKey("  "))
	require.ErrorContains(t, err, "WithAPIKey")
	require.Nil(t, empty)
}
