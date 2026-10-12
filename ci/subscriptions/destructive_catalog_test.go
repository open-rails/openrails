//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

// catalogEditor is a signed-in staff member the host names on its calls.
func catalogEditor(t *testing.T) context.Context {
	return openrails.WithInvoker(t.Context(), openrails.Invoker{Issuer: "https://staff.example", ID: "catalog-editor"})
}

func TestDestructiveCatalogBatchRefusedBeforeAnyChange(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	doc := &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{}}
	for i := range 6 {
		key := fmt.Sprintf("guard-product-%d", i)
		_, err := client.CreateProduct(catalogEditor(t), billing.CreateProductParams{Key: key, DisplayName: key})
		require.NoError(t, err, "creating a product is additive and spends no unit")
		doc.Products[key] = catalog.ApplyProduct{DisplayName: catalog.Value(key), Entitlements: catalog.Value([]string{"guard:batch"})}
	}
	_, err := client.ApplyCatalog(catalogEditor(t), doc, billing.ApplyCatalogParams{})
	require.Error(t, err)
	require.Equal(t, "rate_limit_exceeded", codeOf(err))
	for key := range doc.Products {
		product, err := productByKey(t.Context(), client, key)
		require.NoError(t, err)
		require.Empty(t, product.Entitlements, "a refused whole batch changes no target")
	}
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+w.schema+".destructive_actions").Scan(&count))
	require.Zero(t, count, "refused batch leaves no partly admitted command")
	_, err = client.ApplyCatalog(t.Context(), doc, billing.ApplyCatalogParams{})
	require.NoError(t, err, "the host's own work is neither limited nor held")
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT sum(units) FROM "+w.schema+".destructive_actions WHERE origin='host'").Scan(&count))
	require.Equal(t, 6, count, "and stays in the audit")
}

func TestDestructiveCatalogNoopAndCosmeticSurviveHold(t *testing.T) {
	w := newWorld(t)
	client, staff := w.client[embedded], catalogEditor(t)
	product, err := client.CreateProduct(staff, billing.CreateProductParams{Key: "guard-product", DisplayName: "Guard product"})
	require.NoError(t, err)
	for i := range 5 {
		product, err = client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{fmt.Sprintf("guard:%d", i)})})
		require.NoError(t, err)
	}
	_, err = client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"guard:blocked"})})
	require.Error(t, err)
	require.Equal(t, "rate_limit_exceeded", codeOf(err))
	var units int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT coalesce(sum(units),0) FROM "+w.schema+".destructive_actions WHERE origin='staff' AND actor_id='catalog-editor' AND state='succeeded'").Scan(&units))
	require.EqualValues(t, 5, units)
	// Time does not clear a hold, but unchanged/cosmetic writes need no new unit.
	w.clock.Advance(25 * time.Hour)
	_, err = client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{Entitlements: catalog.Value(product.Entitlements)})
	require.NoError(t, err)
	got, err := client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("New title")})
	require.NoError(t, err)
	require.Equal(t, "New title", got.DisplayName)
	require.Equal(t, product.Entitlements, got.Entitlements)
	_, err = client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.Error(t, err)
	require.Equal(t, "rate_limit_exceeded", codeOf(err))
}

func TestDestructiveConfigurationLostReplyReusesAdmission(t *testing.T) {
	w := prepareWorld(t, 12)
	w.vault = vaulttest.New(t)
	target, err := url.Parse(w.vault.Addr)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var lose atomic.Bool
	proxy.ModifyResponse = func(response *http.Response) error {
		// Vault's client writes with PUT.
		write := response.Request.Method == http.MethodPut || response.Request.Method == http.MethodPost
		if lose.Load() && write && strings.HasSuffix(response.Request.URL.Path, "/merchant") {
			return fmt.Errorf("simulated lost Vault write response")
		}
		return nil
	}
	front := httptest.NewServer(proxy)
	t.Cleanup(front.Close)
	w.vaultAddr = front.URL
	w.start()
	rt := engine.Graph(w.rt).Runtime
	require.NotNil(t, rt.DestructiveAdmissions)
	client := w.client[embedded]
	grace := 9
	request := billing.UpdateMerchantConfigurationParams{Settings: &billing.MerchantSettings{ArrearsGraceDays: &grace}}
	lose.Store(true)
	_, err = client.UpdateMerchantConfiguration(t.Context(), request)
	require.Error(t, err)
	lose.Store(false)
	var before int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT coalesce(sum(units),0) FROM "+w.schema+".destructive_actions WHERE origin='host'").Scan(&before))
	require.EqualValues(t, 1, before)
	got, err := client.UpdateMerchantConfiguration(t.Context(), request)
	require.NoError(t, err)
	require.NotNil(t, got.Settings.ArrearsGraceDays)
	require.Equal(t, 9, *got.Settings.ArrearsGraceDays)
	var count, pending int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*),count(*) FILTER(WHERE state IN ('reserved','started','uncertain')) FROM "+w.schema+".destructive_actions WHERE origin='host'").Scan(&count, &pending))
	require.EqualValues(t, 1, count, "lost response replay uses the original logical command")
	require.Zero(t, pending, "durable readback proves that write succeeded")
}
