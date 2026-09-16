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

// TestInProcessClientBindingIsImmutable: an in-process client names exactly one
// merchant at construction, and a runtime bound to another merchant refuses it
// before any handler runs (#772).
func TestInProcessClientBindingIsImmutable(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}

	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	_, err = rt.Client()
	require.ErrorContains(t, err, "WithMerchantID", "a multi-merchant runtime never guesses the merchant")
	otherID := openrails.MerchantID(uuid.New())
	early, err := rt.Client(openrails.WithMerchantID(otherID))
	require.NoError(t, err)

	slug := fmt.Sprintf("embed-merchant-binding-%d", time.Now().UnixNano())
	boundID, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	customerID := seedCustomerForBoundMerchant(ctx, t, boundID)

	_, err = rt.Client(openrails.WithMerchantID(otherID))
	require.ErrorContains(t, err, boundID.String(), "construction refuses a different merchant")

	for name, call := range map[string]func() error{
		"read": func() error { _, err := early.GetMerchantSettings(ctx); return err },
		"write": func() error {
			return early.SetCustomerSpendDelegations(ctx, customerID.String(), []openrails.SpendDelegationInput{{
				Scope: "invoker", ScopeKey: "test-invoker-mismatch",
				Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
			}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			require.ErrorIs(t, err, openrails.ErrConflict)
			require.Contains(t, err.Error(), boundID.String())
			require.Contains(t, err.Error(), otherID.String())
			var se *openrails.StatusError
			require.True(t, errors.As(err, &se), "must be a *openrails.StatusError, got %T: %v", err, err)
		})
	}

	client, err := rt.Client()
	require.NoError(t, err)
	require.Equal(t, boundID, client.MerchantID())
	_, err = client.GetMerchantSettings(ctx)
	require.NoError(t, err)
	require.NoError(t, client.SetCustomerSpendDelegations(ctx, customerID.String(), []openrails.SpendDelegationInput{{
		Scope: "invoker", ScopeKey: "test-invoker-match",
		Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
	}}))
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
		`INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2)`,
		customerID, uuid.UUID(boundID))
	require.NoError(t, err, "seed customer under the bound merchant")
	return customerID
}
