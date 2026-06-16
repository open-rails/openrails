//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

func TestStandaloneMerchantControlBoundaries(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	merchantB := standalone.ProvisionOwnedMerchant("or502-b")
	customerA := uuid.New()
	customerB := uuid.New()

	// Baseline: the default bootstrapped org controls only the default merchant.
	status, body := postDepositCredits(t, standalone.BaseURL, standalone.Token, customerA, 1_000)
	require.Equal(t, http.StatusOK, status, body)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	// A different org's service token is valid, but it is pinned to that org's
	// own merchant.
	merchantBClient := standalone.Client(openrails.WithTokenProvider(func(context.Context) (string, error) {
		return merchantB.ServiceToken, nil
	}))
	status, body = postDepositCredits(t, standalone.BaseURL, merchantB.ServiceToken, customerB, 2_000)
	require.Equal(t, http.StatusOK, status, body)
	assertBalance(t, ctx, merchantBClient, customerB, 2_000)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	// A service token owned by merchant B's org but explicitly resource-scoped to
	// merchant A is rejected before any handler mutation.
	crossScopedServiceToken := standalone.MintServiceToken(
		merchantB.OrgSlug,
		"or502-cross-service-token",
		controlplane.OperatorRolePermissions(),
		[]authcore.ServiceTokenResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)
	status, body = postDepositCredits(t, standalone.BaseURL, crossScopedServiceToken, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)
	assertBalance(t, ctx, merchantBClient, customerB, 2_000)

	// An AuthKit org with no active OpenRails merchant owns no merchant surface.
	orphanOrg := standalone.CreateAuthKitOrg("or502-orphan")
	orphanToken := standalone.MintServiceToken(
		orphanOrg,
		"or502-orphan-service-token",
		controlplane.OperatorRolePermissions(),
		[]authcore.ServiceTokenResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)
	status, body = postDepositCredits(t, standalone.BaseURL, orphanToken, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)

	// Ordinary user access tokens are not accepted on the server-to-server
	// merchant-control surface.
	userToken := standalone.MintUserAccessToken("or502-user")
	status, body = postDepositCredits(t, standalone.BaseURL, userToken, uuid.New(), 10)
	require.Equal(t, http.StatusUnauthorized, status, body)

	// JWKS remote_application self-tokens use stored AuthKit org authority. A
	// principal with the operator role mutates only its org's merchant; one
	// without a role is denied.
	remoteAppB := standalone.RegisterRemoteApplication("or502-ra-b", merchantB.OrgSlug, controlplane.OperatorRole)
	status, body = postDepositCredits(t, standalone.BaseURL, remoteAppB.Token, customerB, 3_000)
	require.Equal(t, http.StatusOK, status, body)
	assertBalance(t, ctx, merchantBClient, customerB, 5_000)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	remoteAppWithoutRole := standalone.RegisterRemoteApplication("or502-ra-denied", merchantB.OrgSlug, "")
	status, body = postDepositCredits(t, standalone.BaseURL, remoteAppWithoutRole.Token, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)

	// First-party service JWTs resolve by registered issuer -> AuthKit org ->
	// owned merchant. They can act on their own merchant and cannot claim a
	// resource from another merchant.
	serviceJWTB := standalone.RegisterServiceJWTIssuer(
		"or502-jwt-b",
		merchantB.OrgSlug,
		[]string{controlplane.PermCreditsWrite},
		nil,
	)
	status, body = postDepositCredits(t, standalone.BaseURL, serviceJWTB.Token, customerB, 4_000)
	require.Equal(t, http.StatusOK, status, body)
	assertBalance(t, ctx, merchantBClient, customerB, 9_000)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	crossScopedServiceJWT := standalone.RegisterServiceJWTIssuer(
		"or502-jwt-cross",
		merchantB.OrgSlug,
		[]string{controlplane.PermCreditsWrite},
		[]authcore.ServiceTokenResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)
	status, body = postDepositCredits(t, standalone.BaseURL, crossScopedServiceJWT.Token, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)
	assertBalance(t, ctx, merchantBClient, customerB, 9_000)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	standalone.ProvisionMerchantForOrg("or502-b2", merchantB.OrgSlug)
	status, body = postDepositCredits(t, standalone.BaseURL, merchantB.ServiceToken, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)
}

func postDepositCredits(t *testing.T, baseURL, token string, customer uuid.UUID, amount int64) (int, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"customer_id": customer.String(),
		"invoker":     "or502-boundary-test",
		"currency":    "USD",
		"amount":      amount,
		"source":      "or502",
		"source_id":   uuid.NewString(),
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/service/credits/deposit", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func assertBalance(t *testing.T, ctx context.Context, c openrails.Client, customer uuid.UUID, want int64) {
	t.Helper()
	got, err := c.Balance(ctx, customer.String())
	require.NoError(t, err)
	require.Equal(t, want, got.BalanceAmount)
}
