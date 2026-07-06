//go:build integration

// Regression for #768: the transcribed localClient bypasses the in-process
// transport (unlike AdmitBatch, which pins the merchant automatically via the
// transport), so its two methods must pin the bound merchant themselves and
// must not swallow the underlying cause into a bare constant 500 message.
package embed_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
)

// TestLocalClientAdmit covers both bugs in embed/client.go's PERMANENT
// transcription (localClient): (1) single Admit (SingleAdmitter, reachable via
// type assertion on Runtime.Client) used to call c.svc.Admit with the raw ctx,
// while Admitter.Admit starts with merchant.Require(ctx) — every embedded
// single-Admit call failed "merchant required" surfaced as a bare 500
// "admission check failed", with AdmitBatch (through the in-process transport)
// unaffected since the transport pins the merchant itself; (2) both
// transcribed methods collapsed any service failure into that constant
// message with the cause dropped, leaving embedded hosts to debug blind.
func TestLocalClientAdmit(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}

	t.Run("merchant pinned: verdict + spend delegations", func(t *testing.T) {
		pool, err := pgxpool.New(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(pool.Close)

		// The admission gate (spendgate) requires Redis; without it Admit fails
		// before ever reaching merchant.Require, masking the regression this test
		// targets.
		rdb, _ := dbtest.SharedRedisClient(t)

		slug := fmt.Sprintf("embed-localclient-admit-%d", time.Now().UnixNano())
		rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, Redis: rdb}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })

		// Bind the merchant the normal provisioning way (UpsertMerchantConfig),
		// not a raw ctx/runtime-field pin.
		_, err = rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
		require.NoError(t, err)

		customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, "c7c7c7c7-0000-4000-8000-000000000042")

		client := rt.Client()
		admitter, ok := client.(embed.SingleAdmitter)
		require.True(t, ok, "Runtime.Client() must implement embed.SingleAdmitter")

		// Money-bearing single Admit. A fresh zero-balance customer is denied on
		// affordability, but that is a VERDICT (Allowed=false, a DenyCode), never
		// an error. On the unfixed code this call errors with "admission check
		// failed" instead (merchant.Require failing on the raw ctx) — this
		// assertion is the merchant-pin regression check.
		resp, err := admitter.Admit(ctx, openrails.AdmitRequest{
			CustomerID:      customerID.String(),
			Invoker:         "test-invoker",
			InvokerType:     openrails.InvokerTypePayer,
			Currency:        "USD",
			EstimatedAmount: 10_000,
			Source:          "localclient-admit-test",
			RequestID:       "localclient-admit-" + customerID.String(),
		})
		require.NoError(t, err, "embedded single Admit must pin the bound merchant itself, not error")
		require.NotNil(t, resp)
		require.False(t, resp.Allowed, "a fresh zero-balance customer must be a policy deny, not allowed")
		require.NotEmpty(t, resp.DenyCode, "a deny verdict must carry a DenyCode")

		// SetCustomerSpendDelegations shares the transcription path (also pins via
		// merchantCtx) through this exact runtime/provisioning setup.
		err = client.SetCustomerSpendDelegations(ctx, customerID.String(), []openrails.SpendDelegationInput{{
			Scope:    "invoker",
			ScopeKey: "test-invoker",
			Windows:  []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 5_000_000, Currency: "USD"}},
		}})
		require.NoError(t, err, "embedded SetCustomerSpendDelegations must pin the bound merchant itself")
	})

	t.Run("no merchant bound: 500 message names the real cause", func(t *testing.T) {
		rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		// Deliberately no UpsertMerchantConfig: merchantCtx has nothing to pin, so
		// c.svc.Admit's merchant.Require(ctx) fails and the wrapped cause must
		// show up in the message, not just the stable constant prefix.

		client := rt.Client()
		admitter, ok := client.(embed.SingleAdmitter)
		require.True(t, ok)

		_, err = admitter.Admit(ctx, openrails.AdmitRequest{
			CustomerID:      "d8d8d8d8-0000-4000-8000-000000000042",
			Invoker:         "test-invoker",
			InvokerType:     openrails.InvokerTypePayer,
			Currency:        "USD",
			EstimatedAmount: 10_000,
			Source:          "localclient-admit-test",
			RequestID:       "localclient-admit-unbound",
		})
		require.Error(t, err)
		var se *openrails.StatusError
		require.True(t, errors.As(err, &se), "must be a *openrails.StatusError, got %T: %v", err, err)
		require.Equal(t, 500, se.Status)
		require.Contains(t, se.Message, "admission check failed", "stable prefix must be kept")
		require.NotEqual(t, "admission check failed", se.Message, "the cause must be appended, not just the constant prefix")
	})
}
