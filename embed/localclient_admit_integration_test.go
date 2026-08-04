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

	"github.com/google/uuid"
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
		// The admission gate (spendgate) requires Redis; without it Admit fails
		// before ever reaching merchant.Require, masking the regression this test
		// targets.
		rdb, _ := dbtest.SharedRedisClient(t)

		slug := fmt.Sprintf("embed-localclient-admit-%d", time.Now().UnixNano())
		rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, Redis: rdb, River: embedded.RiverManagedByOpenRails()}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })

		// Bind the merchant the normal provisioning way (UpsertMerchantConfig),
		// not a raw ctx/runtime-field pin.
		boundID, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
		require.NoError(t, err)

		customerID := seedCustomerForBoundMerchant(ctx, t, boundID)

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
		rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
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

// TestMerchantPinMismatch is the regression for #772: on an engine already
// bound to a merchant (first UpsertMerchantConfig, #770), an explicit per-call
// pin via openrails.WithMerchant naming a DIFFERENT merchant used to be
// silently ignored — the call executed against the bound merchant instead of
// erroring — in BOTH places that read the bound merchant: the in-process
// transport (embed/transport.go RoundTrip, the wire path underneath
// GetMerchantSettings) and the transcribed localClient path (embed/client.go
// merchantCtx, underneath SetCustomerSpendDelegations). A ctx pin that AGREES
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

	client := rt.Client()

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
