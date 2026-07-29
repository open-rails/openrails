//go:build integration

package embed_test

import (
	"context"
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

// or#868 B3: the embedded client's TRANSCRIBED methods — single Admit and both
// spend-delegation writes — bypass the in-process transport, so they never got
// the MerchantDBConnMW pin the wire path gets. They only called merchant.WithID:
// a context VALUE, which does not scope a database. Under the production
// openrails_app role the payable-customer materialization inside Admit was
// denied outright:
//
//	status=500 admission check failed: ERROR: new row violates row-level
//	security policy for table "customers" (SQLSTATE 42501)
//
// That is live product surface — doujins, hentai0 and cozy-art all consume it.
//
// Unlike the pre-existing embed fixtures, this test seeds its customer under the
// merchant the engine is actually BOUND to, so a failure here is the product's,
// not the fixture's.
func TestEmbeddedTranscribedPathPinsTheMerchantConnection(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}

	rdb, _ := dbtest.SharedRedisClient(t)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, Redis: rdb}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	slug := fmt.Sprintf("or868-b3-%d", time.Now().UnixNano())
	boundID, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)

	// The customer belongs to the BOUND merchant — the only shape in which RLS
	// should permit anything at all. Seeded directly rather than through
	// dbtest.EnsureCustomerIDPgx, which hardcodes the canonical test merchant.
	boundPool := dbtest.SharedMerchantPool(t, uuid.UUID(boundID))
	customerID := uuid.New()
	_, err = boundPool.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		customerID, uuid.UUID(boundID), customerID.String())
	require.NoError(t, err)

	client := rt.Client()

	t.Run("single Admit returns a VERDICT, not a 42501", func(t *testing.T) {
		admitter, ok := client.(embed.SingleAdmitter)
		require.True(t, ok, "Runtime.Client() must implement embed.SingleAdmitter")

		resp, err := admitter.Admit(ctx, openrails.AdmitRequest{
			CustomerID:      customerID.String(),
			Invoker:         "or868-b3-invoker",
			InvokerType:     openrails.InvokerTypePayer,
			Currency:        "USD",
			EstimatedAmount: 10_000,
			Source:          "or868-b3",
			RequestID:       "or868-b3-" + customerID.String(),
		})
		require.NoError(t, err, "the embedded admission seam must not 500 under the production role")
		require.NotNil(t, resp)
		require.False(t, resp.Allowed, "a fresh zero-balance customer is a policy deny")
		require.NotEmpty(t, resp.DenyCode, "a deny verdict must carry a DenyCode")
	})

	t.Run("spend-delegation writes land under the merchant's own scope", func(t *testing.T) {
		require.NoError(t, client.SetCustomerSpendDelegations(ctx, customerID.String(),
			[]openrails.SpendDelegationInput{{
				Scope:    "invoker",
				ScopeKey: "or868-b3-invoker",
				Windows: []openrails.SpendLimitWindow{
					{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"},
				},
			}}))

		var rows int
		require.NoError(t, boundPool.QueryRow(ctx, `
SELECT count(*) FROM openrails.invoker_spend_limits
 WHERE merchant_id = $1 AND customer_id = $2 AND scope_key = $3`,
			uuid.UUID(boundID), customerID, "or868-b3-invoker").Scan(&rows))
		require.Equal(t, 1, rows, "the delegation must be visible inside the bound merchant's RLS scope")
	})

	t.Run("#772 pin mismatch is still refused as a conflict, not swallowed", func(t *testing.T) {
		other := openrails.MerchantID(uuid.New())
		err := client.SetCustomerSpendDelegations(openrails.WithMerchant(ctx, other), customerID.String(), nil)
		require.Error(t, err)
		require.ErrorIs(t, err, openrails.ErrConflict,
			"pinning the connection must not have blurred the merchant-mismatch refusal")
	})
}
