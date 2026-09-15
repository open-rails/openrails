//go:build integration

// Merchant scope must be enforced consistently by every client operation.
package embed_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestMerchantPinMismatch is the regression for #772: on an engine already
// bound to a merchant (first UpsertMerchantConfig, #770), an explicit per-call
// pin via openrails.WithMerchant naming a DIFFERENT merchant used to be
// silently ignored — the call executed against the bound merchant instead of
// erroring — in BOTH places that read the bound merchant: the in-process
// transport (embed/transport.go RoundTrip, the wire path underneath
// GetMerchantSettings) and the shared transport underneath SetCustomerSpendDelegations. A ctx pin that AGREES
// with the bound merchant must keep behaving exactly like an unpinned call.
func TestMerchantPinMismatch(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}

	slug := fmt.Sprintf("embed-merchant-pin-mismatch-%d", time.Now().UnixNano())
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	boundID, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)

	otherID := openrails.MerchantID(uuid.New())
	customerID := seedCustomerForBoundMerchant(ctx, t, boundID)

	client, clientErr := rt.Client()
	if clientErr != nil {
		t.Fatal(clientErr)
	}

	t.Run("wire path: GetMerchantSettings mismatch is refused", func(t *testing.T) {
		mismatchCtx := openrails.WithMerchant(ctx, otherID)
		_, err := client.GetMerchantSettings(mismatchCtx)
		require.Error(t, err)
		require.Contains(t, err.Error(), boundID.String(), "error must name the bound merchant")
		require.Contains(t, err.Error(), otherID.String(), "error must name the pinned merchant")
		var se *openrails.StatusError
		require.True(t, errors.As(err, &se), "must be a *openrails.StatusError, got %T: %v", err, err)
		require.True(t, errors.Is(err, openrails.ErrConflict))
	})

	t.Run("transcribed path: SetCustomerSpendDelegations mismatch is refused", func(t *testing.T) {
		mismatchCtx := openrails.WithMerchant(ctx, otherID)
		err := client.SetCustomerSpendDelegations(mismatchCtx, customerID.String(), []openrails.SpendDelegationInput{{
			Scope:    "invoker",
			ScopeKey: "test-invoker-mismatch",
			Windows:  []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
		}})
		require.Error(t, err)
		require.Contains(t, err.Error(), boundID.String(), "error must name the bound merchant")
		require.Contains(t, err.Error(), otherID.String(), "error must name the pinned merchant")
		var se *openrails.StatusError
		require.True(t, errors.As(err, &se), "must be a *openrails.StatusError, got %T: %v", err, err)
		require.True(t, errors.Is(err, openrails.ErrConflict))
	})

	t.Run("matching pin behaves exactly like unpinned", func(t *testing.T) {
		matchCtx := openrails.WithMerchant(ctx, boundID)

		settings, err := client.GetMerchantSettings(matchCtx)
		require.NoError(t, err)
		unpinnedSettings, err := client.GetMerchantSettings(ctx)
		require.NoError(t, err)
		require.Equal(t, unpinnedSettings, settings)

		err = client.SetCustomerSpendDelegations(matchCtx, customerID.String(), []openrails.SpendDelegationInput{{
			Scope:    "invoker",
			ScopeKey: "test-invoker-match",
			Windows:  []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
		}})
		require.NoError(t, err, "a ctx pin matching the bound merchant must not be refused")
	})
}

// seedCustomerForBoundMerchant materializes the customers row under the merchant
// the engine is BOUND to. dbtest.EnsureCustomerIDPgx hardcodes the canonical test
// merchant, so a runtime bound to its own freshly provisioned merchant would be
// writing to another merchant's customer — which the openrails_app role now
// refuses with a 42501 (and which a superuser harness used to let through).
func seedCustomerForBoundMerchant(ctx context.Context, t *testing.T, boundID openrails.MerchantID) uuid.UUID {
	t.Helper()
	pool := dbtest.SharedMerchantPool(t, uuid.UUID(boundID))
	customerID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		customerID, uuid.UUID(boundID), customerID.String())
	require.NoError(t, err, "seed customer under the bound merchant")
	return customerID
}
