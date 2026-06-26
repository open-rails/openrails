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
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
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

	// A different org's API key is valid, but it is pinned to that org's
	// own merchant.
	merchantBClient := standalone.Client(openrails.WithTokenProvider(func(context.Context) (string, error) {
		return merchantB.APIKey, nil
	}))
	status, body = postDepositCredits(t, standalone.BaseURL, merchantB.APIKey, customerB, 2_000)
	require.Equal(t, http.StatusOK, status, body)
	assertBalance(t, ctx, merchantBClient, customerB, 2_000)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	// #569 (hard cut): API-key resource scopes are gone. A key's merchant identity
	// IS the permission group it was minted under, so a key minted under merchant
	// B's group can only ever act for merchant B — there is no way to mint a "B key
	// scoped to merchant A". The former cross-scope rejection sub-case tested a
	// mechanism that no longer exists; cross-merchant isolation is now proven by the
	// group-based cases above and below (B's key never reaches merchant A).

	// An AuthKit org with no active OpenRails merchant owns no merchant surface.
	orphanOrg := standalone.CreateAuthKitOrg("or502-orphan")
	orphanToken := standalone.MintAPIKey(
		orphanOrg,
		"or502-orphan-API-key",
		nil, // #567: MintAPIKey now mints the merchant owner role (full merchant:*)
	)
	status, body = postDepositCredits(t, standalone.BaseURL, orphanToken, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)

	// #564 (no source discrimination): a user access token IS accepted on the
	// merchant surface — authority is the user's live org-role perms, not the
	// credential type. A user with no merchant grant is denied by PERMISSION (403),
	// never rejected by credential source (was 401).
	userToken := standalone.MintUserAccessToken("or502-user")
	status, body = postDepositCredits(t, standalone.BaseURL, userToken, uuid.New(), 10)
	require.Equal(t, http.StatusForbidden, status, body)

	// JWKS remote_application self-tokens use stored AuthKit org authority. A
	// principal with the org owner role mutates only its org's merchant; one
	// without a role is denied.
	remoteAppB := standalone.RegisterRemoteApplication("or502-ra-b", merchantB.OrgSlug, controlplane.OwnerRole)
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
		[]string{controlplane.PermMerchantCustomerSettingsUpdate},
	)
	status, body = postDepositCredits(t, standalone.BaseURL, serviceJWTB.Token, customerB, 4_000)
	require.Equal(t, http.StatusOK, status, body)
	assertBalance(t, ctx, merchantBClient, customerB, 9_000)
	assertBalance(t, ctx, standalone.Client(), customerA, 1_000)

	// #569 (hard cut): a service JWT's self-asserted resources are ignored — its
	// merchant identity is the permission group its issuer is registered under. A
	// JWT issued for merchant B can never claim merchant A, so the former
	// cross-scope rejection sub-case tested a mechanism that no longer exists. The
	// service-JWT case above (own-merchant success) plus the group-based cases prove
	// the boundary holds.
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
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/merchant/credits/deposit", bytes.NewReader(payload))
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
