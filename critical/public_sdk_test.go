//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// Browser drivers import the built SDK. This server only hosts its assets,
// supplies real fixture identities, and controls external provider/network
// boundaries. Every business response comes from the real OpenRails router.
func publicBrowser(t *testing.T, a *authorityWorld, p *providerLedger, network *standaloneWorld, cases ...string) {
	t.Helper()
	directory, err := filepath.Abs("../sdk/billing-ui")
	require.NoError(t, err)
	app := filepath.Join(directory, "test-results/critical-app/app.js")
	_, err = os.Stat(app)
	require.NoError(t, err, "build the SDK and its critical browser bundle before the default critical suite")
	_, price := publicSale(t, a, "sdk-purchase", 9_990_000, nil)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	_, err = a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1_000_000, Source: "critical-sdk", SourceID: "opening-balance"}})
	require.NoError(t, err)
	compute, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sdk-compute", DisplayName: "SDK compute"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "sdk-units", billing.SetMeterParams{EventType: "sdk-compute", ValueProperty: "units", Aggregation: catalog.AggregationSum, RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: compute.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 10, DivideBy: 1}}})})
	require.NoError(t, err)
	usage, err := a.alpha.RecordUsage(t.Context(), []billing.RecordUsageParams{{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "sdk-compute", Dimensions: map[string]int64{"units": 2}, Source: "critical-sdk", SourceID: "worked-usage"}})
	require.NoError(t, err)
	require.Len(t, usage, 1)
	require.NoError(t, usage[0].Err())
	require.EqualValues(t, 20, usage[0].Event.Amount)
	for _, client := range []*openrails.Client{a.alpha, a.alphaRemote} {
		known, err := client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.EqualValues(t, 9_990_000, known.UnitAmount)
		require.Equal(t, "USD", known.Currency)
	}
	var recurring []billing.PriceID
	for i, key := range []string{"sdk-basic", "sdk-premium"} {
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: ptr("sdk-plans"), TierRank: i + 1, Entitlements: []string{"subscription:read"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: []int64{10_000_000, 20_000_000}[i], Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		recurring = append(recurring, price.ID)
	}
	psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	expired := a.signedToken(t, func(_ map[string]any, claims map[string]any) {
		claims["exp"] = time.Now().Add(-time.Minute).Unix()
		claims["iat"] = time.Now().Add(-time.Hour).Unix()
	})
	configuration := map[string]string{"alice": a.alice.ID, "bob": a.bob.ID, "token": a.aliceToken, "otherToken": a.bobToken, "expiredToken": expired, "price": price.ID.String(), "subscriptionPrice": recurring[0].String(), "upgradePrice": recurring[1].String(), "psp": psps.Items[0].ID.String()}
	var cards *authorityWorld
	var vault *nmimock.Mock
	if slices.Contains(cases, "OR-UI-07") {
		cards = newAuthorityWorld(t, func(w *world) {
			vault = nmimock.New(nmimock.Options{Clock: w.clock.Now})
			t.Cleanup(vault.Close)
			w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
			w.cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: vault.URL()}
			merchant := w.cfg.Merchants["alpha"]
			merchant.PSPs = openrails.PSPs{"nmi": openrails.NMIPSP{Environment: openrails.EnvironmentSandbox, GatewayID: "sdk-vault", SecurityKey: "critical-sdk-vault", TokenizationKey: "critical-sdk-tokenization"}.PSPConfig()}
			merchant.CardVault = "nmi"
			w.cfg.Merchants["alpha"] = merchant
		})
		psps, err := cards.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		configuration["nmiToken"] = cards.aliceToken
		configuration["nmiPSP"] = psps.Items[0].ID.String()
		configuration["nmiCardToken"] = vault.Tokenize(nmimock.Card{Brand: "visa", Last4: "5454", CVV: "M", AVS: "Y"})
	}
	if network != nil {
		configuration["networkToken"] = network.Merchants["alpha"].CustomerToken
		configuration["networkMerchant"] = network.Merchants["alpha"].ID.String()
		configuration["networkCustomer"] = network.Merchants["alpha"].CustomerID.String()
		configuration["networkAccount"] = network.AliceID
	}
	target, err := url.Parse(a.server.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	counts := map[string]int{}
	mux := http.NewServeMux()
	if cards != nil {
		cardURL, err := url.Parse(cards.server.URL)
		require.NoError(t, err)
		cardProxy := httputil.NewSingleHostReverseProxy(cardURL)
		mux.HandleFunc("/__nmi/", func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/__nmi")
			r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/__nmi")
			cardProxy.ServeHTTP(w, r)
		})
	}
	if network != nil {
		networkURL, err := url.Parse(network.URL)
		require.NoError(t, err)
		networkProxy := httputil.NewSingleHostReverseProxy(networkURL)
		mux.HandleFunc("/__network/", func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/__network")
			r.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, "/__network")
			networkProxy.ServeHTTP(w, r)
		})
		mux.HandleFunc("POST /__fixture/network-proof", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Nonce string `json:"nonce"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Nonce == "" {
				http.Error(w, "missing actual challenge nonce", 400)
				return
			}
			body, _ := json.Marshal(map[string]string{"Op": "authorize", "Merchant": "alpha", "Account": "alice", "Nonce": in.Nonce})
			req, err := http.NewRequestWithContext(r.Context(), "POST", network.ControlURL, bytes.NewReader(body))
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			defer response.Body.Close()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.StatusCode)
			_, _ = io.Copy(w, response.Body)
		})
	}
	mux.HandleFunc("GET /__fixture/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(configuration)
	})
	mux.HandleFunc("GET /__fixture/counts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(counts)
	})
	mux.HandleFunc("POST /__fixture/card", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Decline string `json:"decline"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad external card", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242", Decline: in.Decline})})
	})
	mux.HandleFunc("POST /__fixture/authenticate", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			http.Error(w, "bad external proof", 400)
			return
		}
		ok := p.core.Authenticate(in.ID)
		if strings.HasPrefix(in.ID, "seti_") {
			ok = p.core.AuthenticateSetup(in.ID)
		}
		if !ok {
			http.Error(w, "no pending external challenge", 409)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /__critical/app.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, app) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><html><body><script type="module" src="/__critical/app.js"></script></body></html>`))
			return
		}
		mu.Lock()
		counts[r.Method+" "+r.URL.Path]++
		if r.URL.Path == "/v1/config" && r.Header.Get("Authorization") != "" {
			counts["public-config-with-credential"]++
		}
		mu.Unlock()
		fault := r.Header.Get("X-Critical-Fault")
		if fault != "" {
			mu.Lock()
			counts[fault]++
			n := counts[fault]
			mu.Unlock()
			if fault == "hang" {
				<-r.Context().Done()
				return
			}
			if fault == "always-unavailable" || fault == "write-unavailable" || fault == "retry-once" && n == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"type":"api_error","code":"service_unavailable","message":"controlled network boundary outage"}}`))
				return
			}
		}
		proxy.ServeHTTP(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	results := ""
	if root := os.Getenv("OPENRAILS_CRITICAL_REPORT_DIR"); root != "" {
		root, err = filepath.Abs(root)
		require.NoError(t, err)
		root = filepath.Join(root, "browser")
		require.NoError(t, os.MkdirAll(root, 0700))
		results, err = os.MkdirTemp(root, strings.Join(cases, "_")+"-")
		require.NoError(t, err)
	} else {
		results = t.TempDir()
	}
	report := filepath.Join(results, "report.json")
	command := exec.CommandContext(t.Context(), "pnpm", "exec", "playwright", "test", "-c", "playwright.critical.config.ts", "--grep", strings.Join(cases, "|"))
	command.Dir = directory
	command.Env = append(os.Environ(), "OPENRAILS_BROWSER_URL="+server.URL, "OPENRAILS_BROWSER_RESULTS="+results, "OPENRAILS_BROWSER_REPORT="+report)
	output, err := command.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err, "built public SDK browser assertions failed")
	check := exec.CommandContext(t.Context(), "node", "critical/check-results.mjs", report, strings.Join(cases, ","))
	check.Dir = directory
	output, err = check.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err, "every selected executable browser case and method must actually pass")
	if vault != nil {
		require.Empty(t, vault.Vaults(), "actual external vault no longer retains the removed instrument")
		require.Empty(t, vault.Sales(), "saving and removing a card must not purchase anything")
		require.Empty(t, vault.Unexpected())
	}
	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, counts["public-config-with-credential"], "the public configuration call must not transmit a customer's token")
}

func TestPublicSDK(t *testing.T) {
	t.Run("OR-J04-S01/built-client-and-real-HTTP-preserve-domain-outcomes", func(t *testing.T) {
		values := exec.CommandContext(t.Context(), "node", "critical/values.mjs")
		values.Dir = "../sdk/billing-ui"
		output, err := values.CombinedOutput()
		require.NoError(t, err, string(output))
		t.Log(string(output))
		a, p := newPaymentWorld(t, func(w *world) { publicPastClock(w, 0) })
		network := newStandaloneWorld(t)
		publicBrowser(t, a, p, network, "OR-UI-01", "OR-UI-02", "OR-UI-03", "OR-UI-06", "OR-UI-07")
		effects := p.effects(t)
		require.Len(t, effects, 3, "one-off purchase, membership enrollment, and one upgrade")
		amounts := []int64{}
		for _, effect := range effects {
			require.Equal(t, "charge", effect.Kind)
			require.Equal(t, "usd", effect.Currency)
			amounts = append(amounts, effect.Amount)
		}
		require.ElementsMatch(t, []int64{2997, 1000, 1000}, amounts, "exact provider cents for purchase, enrollment, and the unused-period upgrade credit")
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{})
		require.NoError(t, err)
		found := false
		for _, finding := range findings.Items {
			if finding.Type == "life.payment_page.unreachable" {
				found = true
			}
		}
		require.True(t, found, "the SDK's report created its real observable merchant finding")
		linked, err := network.alpha.ListNetworkCustomers(t.Context(), billing.NetworkCustomerListParams{NetworkAccountID: billing.NetworkAccountID(uuid.MustParse(network.AliceID))})
		require.NoError(t, err)
		require.Len(t, linked.Items, 1)
		require.Equal(t, network.Merchants["alpha"].CustomerID, linked.Items[0].ID)
		foreign, err := network.beta.ListNetworkCustomers(t.Context(), billing.NetworkCustomerListParams{NetworkAccountID: billing.NetworkAccountID(uuid.MustParse(network.AliceID))})
		require.NoError(t, err)
		require.Empty(t, foreign.Items)
	})
	t.Run("OR-J04-S03/browser-network-retry-and-cancellation-stay-bounded", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		publicBrowser(t, a, p, nil, "OR-UI-04")
		// The Go transport also honors its declared timeout even when the
		// host's injected HTTP client has none. The accepted request remains
		// blocked at a network boundary until cancellation reaches it.
		arrived := make(chan struct{}, 1)
		slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { arrived <- struct{}{}; <-r.Context().Done() }))
		defer slow.Close()
		remote, err := openrails.NewRemote(slow.URL, openrails.RemoteOptions{Timeout: 100 * time.Millisecond, HTTPClient: slow.Client(), Credentials: map[billing.MerchantID]openrails.RemoteCredential{a.alpha.MerchantID(): {APIKey: a.alphaKey.Secret}}})
		require.NoError(t, err)
		defer remote.Close(context.Background())
		client, err := remote.Merchant(a.alpha.MerchantID())
		require.NoError(t, err)
		_, err = client.ListOrders(t.Context(), billing.OrderListParams{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, err, billing.ErrUnreachable)
		select {
		case <-arrived:
		default:
			t.Fatal("timeout never exercised the accepted network request")
		}
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J04-S04/expired-token-retry-commits-only-one-order", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		publicBrowser(t, a, p, nil, "OR-UI-05")
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.EqualValues(t, 19_980_000, orders.Items[0].Total)
		require.Equal(t, billing.OrderOpen, orders.Items[0].Status)
		require.Empty(t, p.effects(t))
	})
}
