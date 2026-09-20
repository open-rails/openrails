//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/integrationharness"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/permissions"
)

// One real standalone mount owns this workflow's merchant, credentials and data.
// API keys and delegated subjects pass through the production authority chain.
type treasuryWorkflow struct {
	surface    *integrationharness.Surface
	merchant   integrationharness.OwnedMerchant
	client     *openrails.Client
	issuer     *integrationharness.DelegatedIssuer
	hostURL    string
	hostPolicy *sync.Map
}

func newTreasuryWorkflow(t *testing.T) treasuryWorkflow {
	t.Helper()
	h := integrationharness.New(t, context.Background())
	surface := h.StartStandalone("USD", integrationharness.WithConfig(func(cfg *config.Config) {
		cfg.MerchantSource = config.MerchantSourceAPI
		cfg.SecretBackend = config.SecretBackendDB
		cfg.ProviderWriteMode = config.ProviderWriteModeFull
	}))
	owned := surface.ProvisionOwnedMerchant("treasury-" + uuid.NewString()[:8])
	host, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, host.Close(context.Background())) })
	app.HostGraph(host).Runtime.SetConfiguredMerchant(owned.MerchantID)
	policy := new(sync.Map)
	bridge, err := orauthkit.NewDelegatedAuthenticator(embcp.Get(surface.App()).AuthService().Verifier(), owned.MerchantID.String(),
		orauthkit.WithMerchantSlug(owned.MerchantSlug),
		orauthkit.WithPermissionResolver(func(_ context.Context, _ *http.Request, claims verify.Claims) ([]string, error) {
			grants, ok := policy.Load(claims.UserID)
			if !ok {
				return nil, fmt.Errorf("host has no billing policy for the verified subject")
			}
			return grants.([]string), nil
		}),
	)
	require.NoError(t, err)
	handler, err := host.Handler(embed.MountOptions{RouteSets: []embed.RouteSet{embed.RouteSetCustomer}, DelegatedAuthenticator: bridge})
	require.NoError(t, err)
	hostServer := httptest.NewServer(handler)
	t.Cleanup(hostServer.Close)
	return treasuryWorkflow{
		hostURL: hostServer.URL, hostPolicy: policy,
		surface: surface, merchant: owned,
		client: surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID)),
		issuer: surface.RegisterDelegatedIssuer("treasury-issuer-"+uuid.NewString()[:8], owned.MerchantSlug),
	}
}

func requestWorkflowJSON(t *testing.T, method, target, token string, body any) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(payload))
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		require.NoError(t, testauth.Authorize(req, token))
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, raw
}

func (f treasuryWorkflow) actor(t *testing.T, grants []string) (openrails.CustomerID, string) {
	t.Helper()
	core := embcp.Get(f.surface.App()).Core()
	name := "billing-" + uuid.NewString()[:8]
	user, err := core.CreateUser(t.Context(), name+"@example.test", name)
	require.NoError(t, err)
	token, _, err := core.MintAccessToken(t.Context(), user.ID, nil)
	require.NoError(t, err)
	id := openrails.CustomerID(uuid.MustParse(user.ID))
	_, err = f.client.EnsureCustomer(t.Context(), id)
	require.NoError(t, err)
	f.hostPolicy.Store(user.ID, append([]string(nil), grants...))
	return id, token
}

func TestTreasuryAuthorityAndMoneyWorkflow(t *testing.T) {
	f := newTreasuryWorkflow(t)
	customer := openrails.CustomerID(uuid.New())
	_, err := f.client.EnsureCustomer(t.Context(), customer)
	require.NoError(t, err)
	token := f.issuer.Mint(customer.String(), "", "", []string{permissions.CustomerAll})
	status, raw := requestWorkflowJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/customers/"+customer.String()+"/balance?currency=USD", token, nil)
	require.Equal(t, http.StatusUnauthorized, status, string(raw), "a merchant issuer cannot grant customer:* outside its stored authority")
	token = f.issuer.Mint(customer.String(), "", "", nil)
	status, raw = requestWorkflowJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/customers/"+customer.String()+"/balance?currency=USD", token, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw), "a valid credential still needs a customer grant")
	payer, hostToken := f.actor(t, []string{permissions.CustomerAll})
	path := f.hostURL + "/v1/customers/" + payer.String() + "/balance?currency=USD"
	status, raw = requestWorkflowJSON(t, http.MethodGet, path, hostToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	for _, invalid := range []string{"", "not-a-token"} {
		status, raw = requestWorkflowJSON(t, http.MethodGet, path, invalid, nil)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
	}
	_, otherToken := f.actor(t, []string{permissions.CustomerAll})
	status, raw = requestWorkflowJSON(t, http.MethodGet, path, otherToken, nil)
	require.Equal(t, http.StatusForbidden, status, string(raw), "the bridge must retain the verified subject")
	t.Run("payer_scopes", func(t *testing.T) { checkTreasuryScopes(t, f) })
	t.Run("admin_grants", func(t *testing.T) { checkTreasuryAdminGrant(t, f) })
}

func workflowObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	return out
}

func checkTreasuryScopes(t *testing.T, f treasuryWorkflow) {
	ctx := t.Context()
	payer, customerToken := f.actor(t, []string{permissions.CustomerAll})
	merchantPayer := openrails.CustomerID(f.merchant.MerchantID.UUID())
	_, err := f.client.EnsureCustomer(ctx, merchantPayer)
	require.NoError(t, err)
	for _, seed := range []struct {
		payer  openrails.CustomerID
		amount int64
	}{{payer, 3_300_000}, {merchantPayer, 4_200_000}} {
		_, err := f.client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: &seed.payer, Invoker: seed.payer.String(), Currency: "EUR", Amount: seed.amount, Source: "treasury", SourceID: uuid.NewString()})
		require.NoError(t, err)
	}
	_, adminToken := f.actor(t, []string{permissions.MerchantAll, permissions.CustomerAll})
	base := f.hostURL + "/v1/customers/"
	read := func(path, token string) map[string]any {
		t.Helper()
		status, raw := requestWorkflowJSON(t, http.MethodGet, base+path, token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		return workflowObject(t, raw)
	}
	for _, row := range []struct{ who, token, balance string }{
		{payer.String(), customerToken, "3300000"},
		{f.merchant.MerchantSlug, adminToken, "4200000"},
		{merchantPayer.String(), adminToken, "4200000"},
	} {
		require.Equal(t, row.balance, read(row.who+"/balance?currency=EUR", row.token)["balance_amount"])
	}
	transactions := read(f.merchant.MerchantSlug+"/transactions?currency=EUR&limit=5", adminToken)["transactions"].([]any)
	require.Len(t, transactions, 1)
	transaction := transactions[0].(map[string]any)
	require.Equal(t, merchantPayer.String(), transaction["customer_id"])
	require.Equal(t, "4200000", transaction["amount"])
	for _, path := range []string{"/usage?currency=EUR", "/payments", "/invoices", "/payment-methods"} {
		read(f.merchant.MerchantSlug+path, adminToken)
	}
	require.Empty(t, read(payer.String()+"/spend-delegations", customerToken)["delegations"])

	_, readOnly := f.actor(t, []string{permissions.MerchantAll, controlplane.PermCustomerBalanceRead})
	require.Equal(t, "4200000", read(f.merchant.MerchantSlug+"/balance?currency=EUR", readOnly)["balance_amount"])
	for _, row := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, "/collection-payment-method", map[string]any{"currency": "EUR", "payment_method_id": openrails.PaymentMethodID(uuid.New()).String()}},
		{http.MethodGet, "/payment-methods", nil},
		{http.MethodPost, "/checkout", map[string]any{"payment": map[string]any{"rail": "stripe"}}},
		{http.MethodPut, "/spend-delegations", map[string]any{"delegations": []any{}}},
	} {
		status, raw := requestWorkflowJSON(t, row.method, base+f.merchant.MerchantSlug+row.path, readOnly, row.body)
		require.Equal(t, http.StatusForbidden, status, string(raw), "each treasury operation needs its own grant")
	}
	_, sellerOnly := f.actor(t, []string{permissions.MerchantAll})
	_, support := f.actor(t, []string{controlplane.PermMerchantSettingsRead, controlplane.PermMerchantCatalogUpdate, controlplane.PermCustomerBalanceRead})
	for _, row := range []struct{ target, token string }{
		{f.merchant.MerchantSlug, sellerOnly}, {f.merchant.MerchantSlug, support},
		{uuid.NewString(), adminToken}, {uuid.NewString(), customerToken},
		{f.merchant.MerchantSlug, customerToken}, {merchantPayer.String(), customerToken},
	} {
		status, raw := requestWorkflowJSON(t, http.MethodGet, base+row.target+"/balance?currency=EUR", row.token, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw), "grants do not erase payer identity or merchant-admin standing")
	}
	for _, path := range []string{"/products", "/products/" + uuid.NewString() + "/access", "/entitlements/active", "/subscriptions", "/notifications"} {
		status, raw := requestWorkflowJSON(t, http.MethodGet, base+f.merchant.MerchantSlug+path, adminToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw), "consumer routes are absent from customer treasury")
	}
	status, raw := requestWorkflowJSON(t, http.MethodPut, base+f.merchant.MerchantSlug+"/collection-payment-method", adminToken,
		map[string]any{"currency": "EUR", "payment_method_id": openrails.PaymentMethodID(uuid.New()).String()})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.Contains(t, string(raw), "not eligible for invoice collection")
	invoker := uuid.NewString()
	status, raw = requestWorkflowJSON(t, http.MethodPut, base+f.merchant.MerchantSlug+"/spend-delegations", adminToken,
		map[string]any{"delegations": []any{map[string]any{"scope": "invoker", "scope_key": invoker, "windows": []any{map[string]any{"key": "day", "window_seconds": 86400, "limit": "1000", "currency": "EUR"}}}}})
	require.Equal(t, http.StatusOK, status, string(raw))
	delegations := read(f.merchant.MerchantSlug+"/spend-delegations", adminToken)["delegations"].([]any)
	require.Len(t, delegations, 1)
	require.Equal(t, invoker, delegations[0].(map[string]any)["scope_key"])
	require.Empty(t, read(payer.String()+"/spend-delegations", customerToken)["delegations"], "merchant policy must not leak into its customer")
}

func checkTreasuryAdminGrant(t *testing.T, f treasuryWorkflow) {
	payer, _ := f.actor(t, nil)
	admin := f.issuer.Mint(uuid.NewString(), "", "", []string{controlplane.PermMerchantCreditsGrant})
	support := f.issuer.Mint(uuid.NewString(), "", "", []string{controlplane.PermMerchantCustomerSettingsUpdate})
	path := f.surface.BaseURL + "/v1/merchant/customers/" + payer.String() + "/credits"
	source := "admin-grant-" + uuid.NewString()
	expires := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Microsecond)
	body := map[string]any{"currency": "USD", "amount": "5000", "source_id": source, "description": "goodwill", "expires_at": expires.Format(time.RFC3339Nano)}
	status, raw := requestWorkflowJSON(t, http.MethodPost, path, support, body)
	require.Equal(t, http.StatusForbidden, status, string(raw), "customer-settings:update cannot mint balance")
	status, raw = requestWorkflowJSON(t, http.MethodPost, path, admin, body)
	require.Equal(t, http.StatusOK, status, string(raw))
	first := workflowObject(t, raw)
	require.NotEmpty(t, first["id"])
	require.Equal(t, "5000", first["amount"])
	require.Equal(t, false, first["replayed"])
	require.Equal(t, expires.Format(time.RFC3339Nano), first["expires_at"])
	status, raw = requestWorkflowJSON(t, http.MethodPost, path, admin, body)
	require.Equal(t, http.StatusOK, status, string(raw))
	again := workflowObject(t, raw)
	require.Equal(t, first["id"], again["id"])
	require.Equal(t, "5000", again["amount"])
	require.Equal(t, true, again["replayed"])
	body["amount"] = "5001"
	status, raw = requestWorkflowJSON(t, http.MethodPost, path, admin, body)
	require.Equal(t, http.StatusConflict, status, string(raw))
	require.Contains(t, string(raw), "idempotency_key_reused")
	status, raw = requestWorkflowJSON(t, http.MethodPost, path, admin, map[string]any{"currency": "USD", "amount": "5000", "source_id": source + "-epoch", "expires_at": expires.Unix()})
	require.Equal(t, http.StatusBadRequest, status, string(raw), "epoch numbers are not instants")
	for _, row := range []struct {
		source string
		status int
	}{{source, http.StatusOK}, {"unknown-" + uuid.NewString(), http.StatusNotFound}} {
		status, raw = requestWorkflowJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/credits/deposit?customer_id="+payer.String()+"&source_id="+row.source, f.merchant.APIKey, nil)
		require.Equal(t, row.status, status, string(raw))
		if row.status == http.StatusOK {
			got := workflowObject(t, raw)
			require.Equal(t, first["id"], got["id"])
			require.Equal(t, "5000", got["amount"])
			require.Equal(t, true, got["replayed"])
		} else {
			require.Contains(t, string(raw), "deposit_not_found")
		}
	}
}
