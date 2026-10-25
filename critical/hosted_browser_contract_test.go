//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestHostedBrowser(t *testing.T) {
	t.Run("OR-J03-S04/built-iframe-origin-source-order-and-fallback-boundaries", func(t *testing.T) {
		attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><title>Other origin</title>"))
		}))
		t.Cleanup(attacker.Close)
		directory, err := filepath.Abs("../sdk/billing-ui")
		require.NoError(t, err)
		app := filepath.Join(directory, "test-results", "hosted-app", "app.js")
		_, err = os.Stat(app)
		require.NoError(t, err, "critical:build must build the real SDK consumer")
		var fixture atomic.Value
		mux := http.NewServeMux()
		mux.HandleFunc("GET /__fixture/config", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(fixture.Load())
		})
		mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, app) })
		mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.FileServer(http.Dir(filepath.Dir(app))).ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<!doctype html><div id="app"></div><script type="module" src="/app.js"></script>`))
		})
		merchant := httptest.NewServer(mux)
		t.Cleanup(merchant.Close)
		a, p := newPaymentWorld(t, func(w *world) {
			declaration := w.cfg.Merchants["alpha"]
			declaration.Checkout = &billing.MerchantCheckoutSettings{EmbedOrigins: []string{merchant.URL}}
			w.cfg.Merchants["alpha"] = declaration
		})
		page := hostCheckoutPage(t, a.world)
		first, _ := hostPayOrder(t, a, "browser-first", true)
		second, _ := hostPayOrder(t, a, "browser-second", true)
		require.Contains(t, *first.PayURL, page.URL)
		fixture.Store(map[string]any{"urls": []string{*first.PayURL, *second.PayURL}, "orders": []string{first.ID.String(), second.ID.String()}, "attacker": attacker.URL})
		results := t.TempDir()
		if root := os.Getenv("OPENRAILS_CRITICAL_REPORT_DIR"); root != "" {
			root, err = filepath.Abs(root)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(root, 0700))
			results, err = os.MkdirTemp(root, "hosted-browser-")
			require.NoError(t, err)
		}
		report := filepath.Join(results, "report.json")
		cmd := exec.CommandContext(t.Context(), "pnpm", "exec", "playwright", "test", "-c", "playwright.hosted.config.ts")
		cmd.Dir = directory
		cmd.Env = append(os.Environ(), "OPENRAILS_BROWSER_URL="+merchant.URL, "OPENRAILS_BROWSER_RESULTS="+results, "OPENRAILS_BROWSER_REPORT="+report)
		output, err := cmd.CombinedOutput()
		t.Log(string(output))
		require.NoError(t, err)
		raw, err := os.ReadFile(report)
		require.NoError(t, err)
		var receipt struct {
			Stats struct{ Expected, Unexpected, Flaky, Skipped int }
		}
		require.NoError(t, json.Unmarshal(raw, &receipt))
		require.Equal(t, 1, receipt.Stats.Expected)
		require.Zero(t, receipt.Stats.Unexpected)
		require.Zero(t, receipt.Stats.Flaky)
		require.Zero(t, receipt.Stats.Skipped)
		// Completion messages are UI notifications, never financial admission.
		require.Empty(t, p.effects(t))
		for _, id := range []billing.OrderID{first.ID, second.ID} {
			order, err := a.alpha.GetOrder(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.OrderOpen, order.Status)
		}
	})
}
