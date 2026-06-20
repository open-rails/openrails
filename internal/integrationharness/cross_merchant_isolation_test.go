//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// Cross-merchant isolation suite (#534 Phase 2): a credential scoped to merchant A
// can never read merchant B's data, asserted at the HTTP boundary against a server
// running as openrails_app (NOBYPASSRLS) so real RLS enforces. Standalone is
// multi-merchant — the server resolves the merchant per-credential — so one server
// serves both A and B.

type serviceBalanceSnapshot struct {
	CustomerID      string `json:"customer_id"`
	Currency        string `json:"currency"`
	BalanceAmount   int64  `json:"balance_amount"`
	AvailableAmount int64  `json:"available_amount"`
}

type adminBillingProfileSnapshot struct {
	CustomerID    string `json:"customer_id"`
	CreditBalance []struct {
		Currency        string `json:"currency"`
		Balance         int64  `json:"balance"`
		HeldBalance     int64  `json:"held_balance"`
		OutstandingOwed int64  `json:"outstanding_owed_amount"`
	} `json:"credit_balance"`
}

type selfAccountSnapshot struct {
	Currency        string `json:"currency"`
	BalanceAmount   int64  `json:"balance_amount"`
	AvailableAmount int64  `json:"available_amount"`
}

// TestAPIKeyCrossMerchantIsolationHTTP: merchant A's API key sees only
// A's ledger for a payer, never merchant B's balance for the same payer id.
func TestAPIKeyCrossMerchantIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd") // server runs as openrails_app: real RLS enforces

	// Merchant A = the bootstrapped test merchant. Merchant B = freshly provisioned.
	aToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"iso-a-"+uuid.NewString(),
		[]string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite},
		[]authcore.APIKeyResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)
	b := surface.ProvisionOwnedMerchant("iso-b-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	bToken := surface.MintAPIKey(
		b.OrgSlug,
		"iso-b-tok-"+uuid.NewString(),
		[]string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite},
		[]authcore.APIKeyResource{controlplane.MerchantResource(b.MerchantID)},
	)

	// One shared payer id, used against BOTH merchants.
	payer := uuid.NewString()

	deposit := func(token string, amount int64) {
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/service/credits/deposit", token, map[string]any{
			"customer_id": payer,
			"invoker":     "iso-test-invoker",
			"currency":    "usd",
			"amount":      amount,
			"source":      "iso-test",
			"source_id":   uuid.NewString(),
		})
		require.Equalf(t, http.StatusOK, status, "deposit: %s", string(body))
	}
	balance := func(token string) serviceBalanceSnapshot {
		status, body := requestJSON(t, http.MethodGet,
			surface.BaseURL+"/v1/service/credits/balance?currency=usd&customer_id="+payer, token, nil)
		require.Equalf(t, http.StatusOK, status, "balance: %s", string(body))
		var out serviceBalanceSnapshot
		require.NoError(t, json.Unmarshal(body, &out))
		return out
	}

	// B deposits 5000 to the payer; B sees its own balance.
	deposit(bToken, 5000)
	require.EqualValues(t, 5000, balance(bToken).BalanceAmount, "merchant B sees its own deposit")

	// Isolation: A querying the same payer id finds no account under A — a zero
	// balance (200) or absent (404), never B's 5000.
	aStatus, aBody := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/service/credits/balance?currency=usd&customer_id="+payer, aToken, nil)
	if aStatus == http.StatusOK {
		var aSnap serviceBalanceSnapshot
		require.NoError(t, json.Unmarshal(aBody, &aSnap))
		require.EqualValuesf(t, 0, aSnap.BalanceAmount,
			"merchant A must not see merchant B's payer balance: %s", string(aBody))
		require.EqualValuesf(t, 0, aSnap.AvailableAmount,
			"merchant A must not see merchant B's payer available: %s", string(aBody))
	} else {
		require.Equalf(t, http.StatusNotFound, aStatus,
			"merchant A's view of B's payer must be zero or absent, got %d: %s", aStatus, string(aBody))
	}

	// And B is still 5000 — A's read never disturbed B.
	require.EqualValues(t, 5000, balance(bToken).BalanceAmount, "merchant B's ledger unchanged")
}

func TestRemoteApplicationSelfJWTCrossMerchantIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	remoteA := surface.RegisterRemoteApplication(
		"iso-ra-a-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		controlplane.OperatorRole,
	)
	b := surface.ProvisionOwnedMerchant("iso-ra-b-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	bToken := surface.MintAPIKey(
		b.OrgSlug,
		"iso-ra-b-tok-"+uuid.NewString(),
		[]string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite},
		[]authcore.APIKeyResource{controlplane.MerchantResource(b.MerchantID)},
	)
	payer := uuid.NewString()

	depositCredits(t, surface.BaseURL, bToken, payer, 5000)
	require.EqualValues(t, 5000, serviceBalance(t, surface.BaseURL, bToken, payer).BalanceAmount)

	// A remote application access token is pinned from its STORED issuer
	// authority. Reading B's payer id through A returns A's isolated view, never
	// B's balance.
	assertNoServiceBalance(t, surface.BaseURL, remoteA.Token, payer)

	// Mutating with A's token affects A's merchant ledger only, while B's ledger
	// remains unchanged.
	aPayer := uuid.NewString()
	depositCredits(t, surface.BaseURL, remoteA.Token, aPayer, 7000)
	require.EqualValues(t, 7000, serviceBalance(t, surface.BaseURL, remoteA.Token, aPayer).BalanceAmount)
	require.EqualValues(t, 5000, serviceBalance(t, surface.BaseURL, bToken, payer).BalanceAmount)

	// A remote application access token cannot widen authority by claiming permissions in the JWT.
	// With no stored role/grant on the remote_application, even a token claiming
	// read/write permissions is denied before it reaches the service handler.
	claimOnly := surface.RegisterRemoteApplicationWithPermissionsClaim(
		"iso-ra-claim-only-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		"",
		[]string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite},
	)
	status, body := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/service/credits/balance?currency=usd&customer_id="+payer, claimOnly.Token, nil)
	require.Containsf(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status,
		"self-claimed remote_application permissions must not authorize without stored authority: %s", string(body))
}

func TestDelegatedAdminCrossMerchantIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	b := surface.ProvisionOwnedMerchant("iso-admin-b-" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	bToken := surface.MintAPIKey(
		b.OrgSlug,
		"iso-admin-b-tok-"+uuid.NewString(),
		[]string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite},
		[]authcore.APIKeyResource{controlplane.MerchantResource(b.MerchantID)},
	)
	targetUser := uuid.NewString()
	depositCredits(t, surface.BaseURL, bToken, targetUser, 5000)

	adminA := surface.RegisterDelegatedCaller(
		"iso-admin-a-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		uuid.NewString(),
		[]string{controlplane.PermMerchantBillingRead},
	)
	adminB := surface.RegisterDelegatedCaller(
		"iso-admin-b-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		b.OrgSlug,
		uuid.NewString(),
		[]string{controlplane.PermMerchantBillingRead},
	)

	bProfile := adminProfile(t, surface.BaseURL, adminB.Token, targetUser)
	require.Len(t, bProfile.CreditBalance, 1)
	require.EqualValues(t, 5000, bProfile.CreditBalance[0].Balance, "merchant B admin sees B user's balance")

	aProfile := adminProfile(t, surface.BaseURL, adminA.Token, targetUser)
	require.Emptyf(t, aProfile.CreditBalance,
		"merchant A delegated admin must not see B user's credit balance")
}

func TestDelegatedSelfTokenSubjectIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	serviceToken := surface.MintAPIKey(
		dbtest.TestMerchantSlug,
		"iso-self-service-"+uuid.NewString(),
		[]string{controlplane.PermCreditsRead, controlplane.PermCreditsWrite},
		[]authcore.APIKeyResource{controlplane.MerchantResource(dbtest.TestMerchantID)},
	)
	subjectA := uuid.NewString()
	subjectB := uuid.NewString()
	depositCredits(t, surface.BaseURL, serviceToken, subjectA, 1000)
	depositCredits(t, surface.BaseURL, serviceToken, subjectB, 5000)

	selfA := surface.RegisterDelegatedCaller(
		"iso-self-a-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		subjectA,
		nil,
	)
	selfB := surface.RegisterDelegatedCaller(
		"iso-self-b-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		subjectB,
		nil,
	)

	require.EqualValues(t, 5000, selfAccount(t, surface.BaseURL, selfB.Token).BalanceAmount,
		"self token B sees only subject B balance")
	require.EqualValues(t, 1000, selfAccount(t, surface.BaseURL, selfA.Token).BalanceAmount,
		"self token A sees its own balance, not subject B's balance")

	status, body := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/admin/users/"+subjectB, selfA.Token, nil)
	require.Equalf(t, http.StatusForbidden, status,
		"self token must not be usable on :user_id admin routes: %s", string(body))
}

func TestMerchantDirectoryRejectsMerchantAdminUserHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	cp := embcp.Get(surface.app)
	require.NotNil(t, cp, "control plane attached")
	core := cp.Core()
	require.NotNil(t, core, "authkit core")

	username := "merchant-admin-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	email := username + "@example.com"
	user, err := core.CreateUser(ctx, email, username)
	require.NoError(t, err, "create merchant admin user")
	require.NoError(t, core.AddMember(ctx, dbtest.TestMerchantSlug, user.ID), "add user to merchant org")
	require.NoError(t, core.AssignRole(ctx, dbtest.TestMerchantSlug, user.ID, controlplane.OperatorRole), "assign per-merchant admin role")
	token, _, err := core.IssueAccessToken(ctx, user.ID, email, nil)
	require.NoError(t, err, "issue merchant admin user access token")

	status, body := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/admin/merchants/"+dbtest.TestMerchantID.String(), token, nil)
	require.Equalf(t, http.StatusForbidden, status,
		"per-merchant org admin user must not reach global merchant directory: %s", string(body))
}

func depositCredits(t *testing.T, baseURL, token, payer string, amount int64) {
	t.Helper()
	status, body := requestJSON(t, http.MethodPost, baseURL+"/v1/service/credits/deposit", token, map[string]any{
		"customer_id": payer,
		"invoker":     "iso-test-invoker",
		"currency":    "usd",
		"amount":      amount,
		"source":      "iso-test",
		"source_id":   uuid.NewString(),
	})
	require.Equalf(t, http.StatusOK, status, "deposit: %s", string(body))
}

func serviceBalance(t *testing.T, baseURL, token, payer string) serviceBalanceSnapshot {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet,
		baseURL+"/v1/service/credits/balance?currency=usd&customer_id="+payer, token, nil)
	require.Equalf(t, http.StatusOK, status, "balance: %s", string(body))
	var out serviceBalanceSnapshot
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func assertNoServiceBalance(t *testing.T, baseURL, token, payer string) {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet,
		baseURL+"/v1/service/credits/balance?currency=usd&customer_id="+payer, token, nil)
	if status == http.StatusOK {
		var out serviceBalanceSnapshot
		require.NoError(t, json.Unmarshal(body, &out))
		require.EqualValuesf(t, 0, out.BalanceAmount, "isolated balance must be zero: %s", string(body))
		require.EqualValuesf(t, 0, out.AvailableAmount, "isolated available balance must be zero: %s", string(body))
		return
	}
	require.Equalf(t, http.StatusNotFound, status, "isolated balance must be zero or absent: %s", string(body))
}

func adminProfile(t *testing.T, baseURL, token, userID string) adminBillingProfileSnapshot {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, baseURL+"/v1/admin/users/"+userID, token, nil)
	require.Equalf(t, http.StatusOK, status, "admin profile: %s", string(body))
	var out adminBillingProfileSnapshot
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func selfAccount(t *testing.T, baseURL, token string) selfAccountSnapshot {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, baseURL+"/v1/me/account?currency=usd", token, nil)
	require.Equalf(t, http.StatusOK, status, "self account: %s", string(body))
	var out selfAccountSnapshot
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}
