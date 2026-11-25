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

// invoicePaymentBrowser opens alice's open Solana invoice in the built
// InvoicePayment, against the real router, and runs the browser case.
func invoicePaymentBrowser(t *testing.T, browserCase string) (*solanaWorld, billing.InvoiceID) {
	t.Helper()
	directory, err := filepath.Abs("../web/billing-ui")
	require.NoError(t, err)
	app := filepath.Join(directory, "test-results", "invoice-app", "app.js")
	_, err = os.Stat(app)
	require.NoError(t, err, "critical:build must build the invoice payment app")
	s := newSolanaWorld(t, false)
	product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "chain-content", DisplayName: "Chain purchase", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", SellVia: []catalog.SellVia{catalog.SellViaSolana}})
	require.NoError(t, err)
	status, raw := s.request(t, http.MethodPost, "/v1/me/invoices", s.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}}, map[string]string{"Idempotency-Key": "browser-invoice"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceOpen, invoice.Status)

	target, err := url.Parse(s.server.URL)
	require.NoError(t, err)
	api := httputil.NewSingleHostReverseProxy(target)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__fixture/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"invoiceId": invoice.ID.String(), "token": s.aliceToken})
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
		results, err = os.MkdirTemp(root, "invoice-payment-browser-")
		require.NoError(t, err)
	}
	report := filepath.Join(results, "report.json")
	cmd := exec.CommandContext(t.Context(), "pnpm", "exec", "playwright", "test", "-c", "playwright.invoice.config.ts", "--grep", browserCase)
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
	return s, invoice.ID
}

// invoiceAttempts counts the invoice's payment attempts.
func invoiceAttempts(t *testing.T, w *world, id billing.InvoiceID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "invoice_attempts"}.Sanitize()+" WHERE invoice_id = $1", id.UUID()).Scan(&n))
	return n
}

func TestInvoicePaymentBrowser(t *testing.T) {
	t.Run("OR-J04-S78/a-lost-pay-answer-is-read-back-under-its-key-and-a-retry-keeps-it", func(t *testing.T) {
		s, invoice := invoicePaymentBrowser(t, "OR-ORDERPAY-01")
		require.Equal(t, 1, invoiceAttempts(t, s.world, invoice), "the retry was answered as the pay it repeated")
		// The customer's view carries the wallet's next action.
		status, raw := s.request(t, http.MethodGet, "/v1/me/invoices/"+invoice.String(), s.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var current billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &current))
		require.NotNil(t, current.Payment.NextAction)
		require.Equal(t, "solana_pay", current.Payment.NextAction.Type)
	})
	t.Run("OR-J04-S79/an-invoice-awaiting-its-wallet-is-read-and-confirmed-well-under-the-purchase-limit", func(t *testing.T) {
		s, invoice := invoicePaymentBrowser(t, "OR-ORDERPAY-02")
		require.Equal(t, 1, invoiceAttempts(t, s.world, invoice))
		current, err := s.alpha.GetInvoice(t.Context(), invoice)
		require.NoError(t, err)
		require.NotEqual(t, billing.InvoiceComplete, current.Status, "no wallet paid")
	})
}
