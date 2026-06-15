//go:build integration

// JWKS-principal programmatic auth on the standalone control plane (#484).
//
// authkit #76 added a SECOND programmatic credential alongside service tokens: a
// remote_application presenting a SELF-signed token (typ=remote-application-
// access+jwt) whose authority is STORED (assigned tenant role/permissions),
// resolved server-side from the validated `iss` — never self-claimed.
//
// This test drives that path end-to-end through the REAL standalone server +
// real AuthKit control plane (integrationharness.StartStandalone):
//
//   - a JWKS principal holding the operator ROLE on the merchant's owner_tenant
//     can administer the merchant (the #481 role-based authz the service-token
//     path already runs), and
//   - a JWKS principal with NO role/perm on that tenant is DENIED.
//
// Service-token auth is unchanged — this is purely an additive second credential.
package embed_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

func TestStandaloneRemoteApplicationAuth(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")

	// A JWKS principal granted the operator role on the merchant's owner_tenant
	// (the tenant that owns the test merchant). Its STORED authority lets it
	// administer the merchant via the existing #481 role-based authz.
	authorized := standalone.RegisterRemoteApplication(
		"or484-authorized", dbtest.TestTenantSlug, controlplane.OperatorRole)

	// A JWKS principal with NO membership/role on the owner_tenant: it owns no
	// active merchant, so it cannot administer this one (fail closed).
	unauthorized := standalone.RegisterRemoteApplication(
		"or484-unauthorized", dbtest.TestTenantSlug, "")

	t.Run("authorized principal administers the merchant", func(t *testing.T) {
		c := standalone.Client(openrails.WithTokenProvider(
			func(context.Context) (string, error) { return authorized.Token, nil }))

		payer := uuid.New()
		pid := openrails.CustomerID(payer)
		src := uuid.New()
		dep, err := c.DepositCredits(ctx, openrails.DepositCreditsRequest{
			CustomerID:  &pid,
			Invoker:     "or484-test",
			Currency:    "USD",
			Amount:      500_000,
			Source:      "or484",
			SourceID:    &src,
			Description: "jwks-principal deposit",
		})
		require.NoError(t, err, "authorized JWKS principal must administer the merchant")
		require.Equal(t, int64(500_000), dep.Amount)

		bal, err := c.Balance(ctx, payer.String())
		require.NoError(t, err, "authorized JWKS principal balance read")
		require.Equal(t, int64(500_000), bal.BalanceAmount)
	})

	t.Run("unauthorized principal is denied", func(t *testing.T) {
		c := standalone.Client(openrails.WithTokenProvider(
			func(context.Context) (string, error) { return unauthorized.Token, nil }))

		payer := uuid.New()
		pid := openrails.CustomerID(payer)
		src := uuid.New()
		_, err := c.DepositCredits(ctx, openrails.DepositCreditsRequest{
			CustomerID:  &pid,
			Invoker:     "or484-test",
			Currency:    "USD",
			Amount:      500_000,
			Source:      "or484",
			SourceID:    &src,
			Description: "should be denied",
		})
		require.Error(t, err, "JWKS principal without a role on the owner_tenant must be denied")
		require.True(t,
			errorsIsAny(err, openrails.ErrUnauthorized, openrails.ErrDenied),
			"deny must surface as unauthorized/forbidden, got %v", err)
	})

	t.Run("service-token auth still works", func(t *testing.T) {
		// The harness client carries a REAL minted service token; it must keep
		// working unchanged (the JWKS path is purely additive).
		c := standalone.Client()
		payer := uuid.New()
		pid := openrails.CustomerID(payer)
		src := uuid.New()
		_, err := c.DepositCredits(ctx, openrails.DepositCreditsRequest{
			CustomerID:  &pid,
			Invoker:     "or484-test",
			Currency:    "USD",
			Amount:      1_000,
			Source:      "or484",
			SourceID:    &src,
			Description: "service-token still works",
		})
		require.NoError(t, err, "service-token auth must keep working")
	})
}

func errorsIsAny(err error, targets ...error) bool {
	for _, tgt := range targets {
		if errors.Is(err, tgt) {
			return true
		}
	}
	return false
}
