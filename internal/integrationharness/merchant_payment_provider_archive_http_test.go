//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// archiveNMIProbe serves loopback credential and sandbox qualification probes.
// Provider PUT performs these probes. Dark, it answers 503 and counts the attempts — the
// terminated-provider condition an archive must not depend on.
type archiveNMIProbe struct {
	URL  string
	dark atomic.Bool
	hits atomic.Int64
}

func newArchiveNMIProbe(t *testing.T) *archiveNMIProbe {
	t.Helper()
	f := &archiveNMIProbe{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.dark.Load() {
			http.Error(w, "account terminated", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/":
			require.NoError(t, r.ParseForm())
			require.NotEmpty(t, r.Form.Get("security_key"))
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
		case "/payments/auth":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"archive-probe","response":"1","response_text":"PROBE","response_code":"100"}`))
		case "/payments/archive-probe/void":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"archive-probe","response":"1","response_text":"SUCCESS"}`))
		default:
			t.Errorf("unexpected NMI probe request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected probe", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// providerArchiveSurface is one deployment's merchant provider surface: the
// standalone server over its API key, or the embedded Handler mount.
type providerArchiveSurface struct {
	name   string
	mid    merchant.ID
	client *openrails.Client
	call   func(t *testing.T, method, path string, body any) (int, []byte)
	// bind writes the price's per-account NMI bindings (#993: bindings hang
	// off the PSP uuid) — the standby's catalog objects, which the runbook
	// prepares before the cutover; the API binds by PSP key only.
	bind func(t *testing.T, price openrails.PriceID, accounts ...uuid.UUID)
}

type providerEnvelope struct {
	PaymentProvider merchants.PaymentProviderConfig `json:"payment_provider"`
}

type providerErrorEnvelope struct {
	Error struct {
		Code     string         `json:"code"`
		Metadata map[string]any `json:"metadata"`
	} `json:"error"`
}

func nmiAccountPair() (string, string) {
	base := uuid.NewString()
	return base + "-a", base + "-b"
}

func armArchiveNMI(t *testing.T, s providerArchiveSurface, accountID string) merchants.PaymentProviderConfig {
	t.Helper()
	status, body := s.call(t, http.MethodPut, "/v1/merchant/payment-providers/nmi", map[string]any{
		"operation_id":      uuid.NewString(),
		"expected_revision": 0,
		"account_id":        accountID,
		"credentials": map[string]string{
			"security_key": "archive-key-" + accountID,
		},
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var out providerEnvelope
	require.NoError(t, json.Unmarshal(body, &out))
	require.False(t, out.PaymentProvider.Archived)
	require.NotEqual(t, uuid.Nil, out.PaymentProvider.ID)
	return out.PaymentProvider
}

func archiveAccount(t *testing.T, s providerArchiveSurface, rail string, id uuid.UUID, body any) (int, []byte) {
	t.Helper()
	return s.call(t, http.MethodPost, "/v1/merchant/payment-providers/"+rail+"/accounts/"+id.String()+"/archive", body)
}

func decodeProvider(t *testing.T, body []byte) merchants.PaymentProviderConfig {
	t.Helper()
	var out providerEnvelope
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out.PaymentProvider
}

func decodeProviderError(t *testing.T, body []byte) providerErrorEnvelope {
	t.Helper()
	var out providerErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out
}

func listProviders(t *testing.T, s providerArchiveSurface, status string) []merchants.PaymentProviderConfig {
	t.Helper()
	code, body := s.call(t, http.MethodGet, "/v1/merchant/payment-providers?provider=nmi&status="+status, nil)
	require.Equal(t, http.StatusOK, code, string(body))
	var out struct {
		Data []merchants.PaymentProviderConfig `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &out), string(body))
	return out.Data
}

func providerIDs(items []merchants.PaymentProviderConfig) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// nmiOptionPSPs runs the real checkout routing for price and returns the
// psp ids of every nmi option it would offer a buyer.
func nmiOptionPSPs(t *testing.T, ctx context.Context, client *openrails.Client, price openrails.PriceID) []string {
	t.Helper()
	options, err := client.ListCheckoutRailOptions(ctx, (price).String())
	require.NoError(t, err)
	var out []string
	for _, option := range options {
		if option.Rail == "nmi" {
			out = append(out, option.PSPID)
		}
	}
	return out
}

// New recurring agreements use a saved NMI method and return a priced quote.
func seedNMIPrice(t *testing.T, ctx context.Context, client *openrails.Client) openrails.PriceID {
	t.Helper()
	key := "archive-" + uuid.NewString()[:8]
	product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: key, DisplayName: "Archive lifecycle"})
	require.NoError(t, err)
	duration := 720
	price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: key + "-recurring", UnitAmount: 5_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
	require.NoError(t, err)
	return sdkPriceID(t, price.ID)
}

func nmiPriceBinder(h *Harness, mid merchant.ID) func(t *testing.T, price openrails.PriceID, accounts ...uuid.UUID) {
	return func(t *testing.T, price openrails.PriceID, accounts ...uuid.UUID) {
		t.Helper()
		links := make(map[string]map[string]string, len(accounts))
		for _, account := range accounts {
			links[account.String()] = map[string]string{"rail": "nmi"}
		}
		require.NoError(t, catalog.NewPriceService(h.MerchantDB(mid.UUID())).UpdatePSPLinks(merchant.WithID(h.ctx, mid), price.UUID(), links))
	}
}

// runProviderArchiveLifecycle is the #655/#656 account lifecycle over one
// deployment: an explicit per-account archive that never contacts the
// provider, a rail-level DELETE that fails closed on ambiguity, a last-active
// refusal with an explicit override, idempotent repeats, kept identity, and
// new checkout resolving only to the remaining active account.
func runProviderArchiveLifecycle(t *testing.T, ctx context.Context, s providerArchiveSurface, probe *archiveNMIProbe) {
	t.Helper()
	accountA, accountB := nmiAccountPair()

	a := armArchiveNMI(t, s, accountA)
	price := seedNMIPrice(t, ctx, s.client)
	s.bind(t, price, a.ID)
	require.Equal(t, []string{a.ID.String()}, nmiOptionPSPs(t, ctx, s.client, price), "checkout resolves to the only active account")

	b := armArchiveNMI(t, s, accountB)
	require.NotEqual(t, a.ID, b.ID)
	s.bind(t, price, a.ID, b.ID)
	require.Equal(t, []string{b.ID.String()}, nmiOptionPSPs(t, ctx, s.client, price), "#655: with two active accounts new work selects the newest")

	// Rail-level DELETE with two active accounts fails closed instead of
	// archiving the newest one (which would be the warm standby, not A).
	status, body := s.call(t, http.MethodDelete, "/v1/merchant/payment-providers/nmi", nil)
	require.Equal(t, http.StatusConflict, status, string(body))
	refused := decodeProviderError(t, body)
	require.Equal(t, "provider_accounts_ambiguous", refused.Error.Code)
	require.Equal(t, "nmi", refused.Error.Metadata["rail"])
	require.Len(t, refused.Error.Metadata["accounts"], 2, string(body))
	require.ElementsMatch(t, []uuid.UUID{a.ID, b.ID}, providerIDs(listProviders(t, s, "active")))

	// Provider A is terminated: its NMI query API answers 5xx. The PUT route
	// re-probes stored credentials before writing, so it cannot archive A.
	probe.dark.Store(true)
	probe.hits.Store(0)
	status, body = s.call(t, http.MethodPut, "/v1/merchant/payment-providers/nmi", map[string]any{"operation_id": uuid.NewString(), "expected_revision": a.Revision, "account_id": accountA, "enabled": false})
	require.NotEqual(t, http.StatusOK, status, "PUT enabled:false probes the dark provider and fails: %s", string(body))
	require.Positive(t, probe.hits.Load(), "PUT probes the provider")
	require.ElementsMatch(t, []uuid.UUID{a.ID, b.ID}, providerIDs(listProviders(t, s, "active")), "the failed PUT changed nothing")

	// The explicit archive needs no provider: zero requests reach it.
	probe.hits.Store(0)
	status, body = archiveAccount(t, s, "nmi", a.ID, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	archivedA := decodeProvider(t, body)
	require.Equal(t, a.ID, archivedA.ID, "#993: archive keeps the immutable account id")
	require.Equal(t, accountA, archivedA.AccountID)
	require.True(t, archivedA.Archived)
	require.NotNil(t, archivedA.ReplacedAt)
	require.True(t, archivedA.Credentials["security_key"].Configured, "credentials stay for the drain")
	require.Zero(t, probe.hits.Load(), "archive never contacts the provider")

	// Idempotent: the repeat returns the same row and still touches nothing.
	status, body = archiveAccount(t, s, "nmi", a.ID, map[string]any{})
	require.Equal(t, http.StatusOK, status, string(body))
	again := decodeProvider(t, body)
	require.True(t, again.Archived)
	require.True(t, archivedA.ReplacedAt.Equal(*again.ReplacedAt), "replaced_at is set once")
	require.Zero(t, probe.hits.Load())

	// #655 invariant: new checkout resolves only to the non-archived account.
	require.Equal(t, []string{b.ID.String()}, nmiOptionPSPs(t, ctx, s.client, price))
	buyer, method := uuid.New(), uuid.New()
	pool := dbtest.SharedMerchantPool(t, s.mid.UUID())
	_, err := pool.Exec(ctx, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, s.mid.UUID(), buyer)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,initial_transaction_id,rail_customer_ref,rail_method_ref,last_four,card_type) VALUES($1,$2,$3,$4,'nmi',$5,$6,$7,'4242','visa')`, method, s.mid.UUID(), buyer, b.ID, method.String(), buyer.String(), method.String())
	require.NoError(t, err)
	session, err := s.client.CreateCheckoutSession(ctx, openrails.CreateCheckoutSessionRequest{
		Customer:       openrails.CheckoutCustomerIdentity{ID: openrails.CustomerID(buyer).String(), VerifiedEmail: "buyer-" + uuid.NewString()[:8] + "@example.test", Username: "buyer" + uuid.NewString()[:8]},
		PriceID:        price.String(),
		IdempotencyKey: uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "nmi", PaymentMethodID: openrails.PaymentMethodID(method).String(), NameOnCard: "Archive Buyer", Zip: "90210", Country: "US"},
	})
	require.NoError(t, err)
	var sessionPSP uuid.UUID
	require.NoError(t, dbtest.SharedMerchantPool(t, s.mid.UUID()).QueryRow(ctx,
		`SELECT psp_id FROM billing.checkout_sessions WHERE merchant_id = $1 AND id = $2`, s.mid.UUID(), uuid.MustParse(strings.TrimPrefix(session.ID, "cs_"))).Scan(&sessionPSP))
	require.Equal(t, b.ID, sessionPSP, "the new session is pinned to the active account")
	require.NotNil(t, session.Amount)
	require.EqualValues(t, 5_000_000, *session.Amount)
	require.Zero(t, probe.hits.Load(), "a saved-method quote does not call the provider")

	// B is now the rail's only active account: refused without the override.
	status, body = archiveAccount(t, s, "nmi", b.ID, nil)
	require.Equal(t, http.StatusConflict, status, string(body))
	refused = decodeProviderError(t, body)
	require.Equal(t, "provider_account_last_active", refused.Error.Code)
	require.Equal(t, b.ID.String(), refused.Error.Metadata["psp_id"])
	require.Equal(t, []uuid.UUID{b.ID}, providerIDs(listProviders(t, s, "active")))

	status, body = archiveAccount(t, s, "nmi", b.ID, map[string]any{"allow_last": true})
	require.Equal(t, http.StatusOK, status, string(body))
	require.True(t, decodeProvider(t, body).Archived)
	require.Empty(t, listProviders(t, s, "active"))
	require.ElementsMatch(t, []uuid.UUID{a.ID, b.ID}, providerIDs(listProviders(t, s, "archived")), "archive is not deletion")
	require.Empty(t, nmiOptionPSPs(t, ctx, s.client, price), "no archived account is offered to a buyer")

	// Nothing active on the rail: the rail-level archive has nothing to do.
	status, body = s.call(t, http.MethodDelete, "/v1/merchant/payment-providers/nmi", nil)
	require.Equal(t, http.StatusNotFound, status, string(body))

	// Identity is exact: wrong rail, unknown id, malformed id, unknown field.
	status, body = archiveAccount(t, s, "stripe", a.ID, nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	status, body = archiveAccount(t, s, "nmi", uuid.New(), nil)
	require.Equal(t, http.StatusNotFound, status, string(body))
	status, body = s.call(t, http.MethodPost, "/v1/merchant/payment-providers/nmi/accounts/not-a-uuid/archive", nil)
	require.Equal(t, http.StatusBadRequest, status, string(body))
	status, body = archiveAccount(t, s, "nmi", a.ID, map[string]any{"force": true})
	require.Equal(t, http.StatusBadRequest, status, string(body))
	probe.dark.Store(false)
}

// runProviderArchiveRace archives two active accounts concurrently without the
// override: the rail keeps at least one active account.
func runProviderArchiveRace(t *testing.T, s providerArchiveSurface) {
	t.Helper()
	accountA, accountB := nmiAccountPair()
	a := armArchiveNMI(t, s, accountA)
	b := armArchiveNMI(t, s, accountB)

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i, id := range []uuid.UUID{a.ID, b.ID} {
		wg.Add(1)
		go func(i int, id uuid.UUID) {
			defer wg.Done()
			statuses[i], _ = s.call(t, http.MethodPost, "/v1/merchant/payment-providers/nmi/accounts/"+id.String()+"/archive", nil)
		}(i, id)
	}
	wg.Wait()
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	require.Len(t, listProviders(t, s, "active"), 1, "one account survives the race")
	// Leave the fixture merchant with nothing armed on the rail.
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		status, body := archiveAccount(t, s, "nmi", id, map[string]any{"allow_last": true})
		require.Equal(t, http.StatusOK, status, string(body))
	}
}

func TestStandaloneProviderAccountArchiveLifecycle(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("USD")
	probe := newArchiveNMIProbe(t)
	surface.App().Runtime.Merchants.SetCredentialProbeEndpointsForIntegration(probe.URL, "")
	surface.App().Runtime.Merchants.SetNMIProbeV5EndpointForIntegration(probe.URL)

	owned := surface.ProvisionOwnedMerchant("l22arch" + uuid.NewString()[:8])
	s := providerArchiveSurface{
		name:   "standalone",
		mid:    owned.MerchantID,
		client: surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID)),
		call: func(t *testing.T, method, path string, body any) (int, []byte) {
			return requestJSON(t, method, surface.BaseURL+path, owned.APIKey, body)
		},
		bind: nmiPriceBinder(h, owned.MerchantID),
	}
	runProviderArchiveLifecycle(t, ctx, s, probe)
	runProviderArchiveRace(t, s)

	// Another merchant cannot archive (or see) this merchant's account.
	accountA, _ := nmiAccountPair()
	a := armArchiveNMI(t, s, accountA)
	other := surface.ProvisionOwnedMerchant("l22other" + uuid.NewString()[:8])
	status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/payment-providers/nmi/accounts/"+a.ID.String()+"/archive", other.APIKey, map[string]any{"allow_last": true})
	require.Equal(t, http.StatusNotFound, status, string(body))
	require.Equal(t, []uuid.UUID{a.ID}, providerIDs(listProviders(t, s, "active")))
}

type archiveGate struct{ id merchant.ID }

func (g archiveGate) Authorize(context.Context, *http.Request, string) (billingauth.Principal, error) {
	return billingauth.Principal{MerchantID: g.id}, nil
}

// The embedded Handler mount (RouteSetMerchantConfig) serves the identical
// lifecycle: same routes, same codes, same checkout consequence.
func TestEmbeddedProviderAccountArchiveLifecycle(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	slug := fmt.Sprintf("l22emb%d", time.Now().UnixNano())
	rt, err := embed.New(ctx, embed.Options{
		Merchant: &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{DisplayName: slug}},
		Config: &config.Config{Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
			TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true, AllowCatalogUpdates: true,
			SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull,
			DB: &config.DBConfig{URL: h.DSN},
		},
		Redis: h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	boundClient, err := rt.Client()
	require.NoError(t, err)
	mid := boundClient.MerchantID()
	runtime := app.HostGraph(rt).Runtime
	require.NoError(t, runtime.EnsureMerchantsService(ctx))
	probe := newArchiveNMIProbe(t)
	runtime.Merchants.SetCredentialProbeEndpointsForIntegration(probe.URL, "")
	runtime.Merchants.SetNMIProbeV5EndpointForIntegration(probe.URL)

	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{MerchantConfig: true}, Gate: archiveGate{id: mid}})
	require.NoError(t, err)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := rt.Client(openrails.WithMerchantID(mid))
	require.NoError(t, err)

	s := providerArchiveSurface{
		name:   "embedded",
		mid:    mid,
		client: client,
		call: func(t *testing.T, method, path string, body any) (int, []byte) {
			return requestJSON(t, method, srv.URL+path, "", body)
		},
		bind: nmiPriceBinder(h, mid),
	}
	runProviderArchiveLifecycle(t, ctx, s, probe)
	runProviderArchiveRace(t, s)
}
