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
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// orderPaymentBrowser opens alice's open Solana order in the built
// OrderPayment, against the real router, and runs the browser case.
func orderPaymentBrowser(t *testing.T, browserCase string) (*solanaWorld, billing.OrderID) {
	t.Helper()
	directory, err := filepath.Abs("../web/billing-ui")
	require.NoError(t, err)
	app := filepath.Join(directory, "test-results", "order-app", "app.js")
	_, err = os.Stat(app)
	require.NoError(t, err, "critical:build must build the order payment app")
	s := newSolanaWorld(t, false)
	product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "chain-content", DisplayName: "Chain purchase", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", SellVia: []catalog.SellVia{catalog.SellViaSolana}})
	require.NoError(t, err)
	status, raw := s.request(t, http.MethodPost, "/v1/me/orders", s.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}}, map[string]string{"Idempotency-Key": "browser-order"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderOpen, order.Status)

	target, err := url.Parse(s.server.URL)
	require.NoError(t, err)
	api := httputil.NewSingleHostReverseProxy(target)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__fixture/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"orderId": order.ID.String(), "token": s.aliceToken})
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, app) })
	mux.Handle("/v1/", api)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.FileServer(http.Dir(filepath.Dir(app))).ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><div id="app"></div><script type="module" src="/app.js"></script>`))
	})
	page := httptest.NewServer(mux)
	t.Cleanup(page.Close)
	results := t.TempDir()
	if root := os.Getenv("OPENRAILS_CRITICAL_REPORT_DIR"); root != "" {
		root, err = filepath.Abs(root)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(root, 0700))
		results, err = os.MkdirTemp(root, "order-payment-browser-")
		require.NoError(t, err)
	}
	report := filepath.Join(results, "report.json")
	cmd := exec.CommandContext(t.Context(), "pnpm", "exec", "playwright", "test", "-c", "playwright.order.config.ts", "--grep", browserCase)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "OPENRAILS_BROWSER_URL="+page.URL, "OPENRAILS_BROWSER_RESULTS="+results, "OPENRAILS_BROWSER_REPORT="+report)
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
	raw, err = os.ReadFile(report)
	require.NoError(t, err)
	var receipt struct {
		Stats struct{ Expected, Unexpected, Flaky, Skipped int }
	}
	require.NoError(t, json.Unmarshal(raw, &receipt))
	require.Equal(t, 1, receipt.Stats.Expected)
	require.Zero(t, receipt.Stats.Unexpected)
	require.Zero(t, receipt.Stats.Flaky)
	require.Zero(t, receipt.Stats.Skipped)
	return s, order.ID
}

// orderAttempts counts the order's payment attempts.
func orderAttempts(t *testing.T, w *world, id billing.OrderID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "order_attempts"}.Sanitize()+" WHERE order_id = $1", id.UUID()).Scan(&n))
	return n
}

func TestOrderPaymentBrowser(t *testing.T) {
	t.Run("OR-J04-S78/a-lost-pay-answer-is-read-back-under-its-key-and-a-retry-keeps-it", func(t *testing.T) {
		s, order := orderPaymentBrowser(t, "OR-ORDERPAY-01")
		require.Equal(t, 1, orderAttempts(t, s.world, order), "the retry was answered as the pay it repeated")
		// The customer's view carries the wallet's next action.
		status, raw := s.request(t, http.MethodGet, "/v1/me/orders/"+order.String(), s.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var current billing.Order
		require.NoError(t, json.Unmarshal(raw, &current))
		require.NotNil(t, current.Payment.NextAction)
		require.Equal(t, "solana_pay", current.Payment.NextAction.Type)
	})
	t.Run("OR-J04-S79/an-order-awaiting-its-wallet-is-read-and-confirmed-well-under-the-purchase-limit", func(t *testing.T) {
		s, order := orderPaymentBrowser(t, "OR-ORDERPAY-02")
		require.Equal(t, 1, orderAttempts(t, s.world, order))
		current, err := s.alpha.GetOrder(t.Context(), order)
		require.NoError(t, err)
		require.NotEqual(t, billing.OrderComplete, current.Status, "no wallet paid")
	})
}
