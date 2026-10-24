//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// Browser drivers import the built SDK. This server only hosts its assets,
// supplies real fixture identities, and controls external provider/network
// boundaries. Every business response comes from the real OpenRails router.
func publicBrowser(t *testing.T, a *authorityWorld, p *providerLedger, cases ...string) {
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
	target, err := url.Parse(a.server.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	counts := map[string]int{}
	mux := http.NewServeMux()
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
	results := t.TempDir()
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
	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, counts["public-config-with-credential"], "the public configuration call must not transmit a customer's token")
}

func TestPublicSDK(t *testing.T) {
	t.Run("OR-J04-S01/built-client-and-real-HTTP-preserve-domain-outcomes", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		publicBrowser(t, a, p, "OR-UI-01", "OR-UI-02", "OR-UI-03")
		effects := p.effects(t)
		require.Len(t, effects, 3, "one-off purchase, membership enrollment, and one upgrade")
		for _, effect := range effects {
			require.Equal(t, "charge", effect.Kind)
			require.Equal(t, "usd", effect.Currency)
		}
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{})
		require.NoError(t, err)
		found := false
		for _, finding := range findings.Items {
			if finding.Type == "life.payment_page.unreachable" {
				found = true
			}
		}
		require.True(t, found, "the SDK's report created its real observable merchant finding")
	})
	t.Run("OR-J04-S03/browser-network-retry-and-cancellation-stay-bounded", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		publicBrowser(t, a, p, "OR-UI-04")
		require.Empty(t, p.effects(t))
	})
	t.Run("OR-J04-S04/expired-token-retry-commits-only-one-order", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		publicBrowser(t, a, p, "OR-UI-05")
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.EqualValues(t, 19_980_000, orders.Items[0].Total)
		require.Equal(t, billing.OrderOpen, orders.Items[0].Status)
		require.Empty(t, p.effects(t))
	})
}
