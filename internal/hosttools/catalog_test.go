package hosttools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogrules"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
)

// The shipped examples are the single-merchant application contract; the main
// example keeps its metered matrix, monthly cap and pooled allowance.
func TestExampleCatalogApplicationsParse(t *testing.T) {
	var metered *catalog.Application
	for _, name := range []string{"catalog.example.yaml", "catalog.subscriptions.example.yaml", "catalog.membership.example.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", name))
		require.NoError(t, err)
		application, err := catalog.ParseApplicationYAML(raw)
		require.NoError(t, err, name)
		require.NotEmpty(t, application.Products, name)
		require.False(t, application.Prune, "an example preserves omitted items: %s", name)
		if name == "catalog.example.yaml" {
			metered = application
		}
	}
	require.NotEmpty(t, metered.Meters)
	var matrix *catalog.RatePrice
	allowance := false
	for key, product := range metered.Products {
		for i := range product.RateCards.Value {
			card := &product.RateCards.Value[i]
			allowance = allowance || card.Allowance != nil
			if key == "droplet" && card.Price.PerUnit != nil && card.Price.PerUnit.Matrix != nil {
				require.False(t, product.TierGroup.Set, "usage products are not tier subscriptions")
				matrix = &card.Price
			}
		}
	}
	require.True(t, allowance, "pooled egress allowance survives")
	require.NotNil(t, matrix)
	model, ok := catalogrules.ForCell(*matrix, "s-1vcpu-1gb")
	require.True(t, ok)
	cost, err := model.Rate(720 * 3600)
	require.NoError(t, err)
	require.Equal(t, int64(6000000), cost, "a full month is capped at the monthly price")
}

// The operator selects the merchant; the document can never select authority.
func TestApplyMerchantCatalogPreconditions(t *testing.T) {
	const header = "schema_version: 1\n"
	for _, field := range []string{"merchant: another-merchant", "merchants: []", "auth: {}", "catalogs: []"} {
		_, err := ApplyMerchantCatalog(context.Background(), CatalogApplyOptions{Merchant: "operator-selected", Manifest: []byte(header + field + "\n")})
		require.ErrorContains(t, err, "unknown field", field)
	}
	_, err := ApplyMerchantCatalog(context.Background(), CatalogApplyOptions{Merchant: " ", Manifest: []byte(header)})
	require.ErrorContains(t, err, "merchant is required")
	_, err = ApplyMerchantCatalog(context.Background(), CatalogApplyOptions{Merchant: "m", File: "catalog.yaml", Manifest: []byte(header)})
	require.ErrorContains(t, err, "not both")
	_, err = ApplyMerchantCatalog(context.Background(), CatalogApplyOptions{Merchant: "m", Manifest: []byte(header)})
	require.ErrorContains(t, err, "configuration is required", "a valid document without a host runtime or config opens nothing")
}

// A host-supplied runtime keeps its armed credential plane; catalog apply
// never builds a second graph beside it.
func TestCatalogRuntimeReusesHostRuntime(t *testing.T) {
	hostRuntime := &app.Runtime{MoneyService: &money.MoneyService{}, EntitlementService: &entitlements.EntitlementService{}}
	rt, svc, cleanup, err := catalogRuntime(context.Background(), CatalogApplyOptions{App: &app.App{Runtime: hostRuntime}})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.Same(t, hostRuntime, rt)
	require.NotNil(t, svc)

	_, _, _, err = catalogRuntime(context.Background(), CatalogApplyOptions{App: &app.App{}})
	require.ErrorContains(t, err, "not initialized")
}

// Dumps are manifest input: storage stamps and resolved on-chain snapshots
// are dropped, and a USDC default or a plan_pda makes token redundant.
func TestProviderLinksDumpOnlyDeclarativeFields(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want map[string]map[string]string
	}{
		"attached plan":   {`{"solana":{"rail":"solana","token":"USD1","mint_symbol":"USD1","plan_pda":"plan"}}`, map[string]map[string]string{"solana": {"plan_pda": "plan"}}},
		"default token":   {`{"solana":{"rail":"solana","token":"usdc","mint_symbol":"USDC"}}`, map[string]map[string]string{"solana": {}}},
		"creation intent": {`{"solana":{"rail":"solana","token":"USD1"}}`, map[string]map[string]string{"solana": {"token": "USD1"}}},
		"other rail":      {`{"mobius":{"rail":"nmi","plan_id":"premium","token":"kept"}}`, map[string]map[string]string{"mobius": {"plan_id": "premium", "token": "kept"}}},
		"empty":           {`{}`, nil},
		"named account":   {`{"chain-main":{"rail":"solana","psp_id":"account","mint_symbol":"USD1","token":"USD1","plan_pda":"plan"}}`, map[string]map[string]string{"chain-main": {"psp_id": "account", "plan_pda": "plan"}}},
	} {
		got, err := providerLinks([]byte(tc.raw))
		require.NoError(t, err, name)
		require.Equal(t, tc.want, got, name)
	}
}

// A malformed stored binding must not become a plausible link-free export.
func TestProviderLinksDumpRefusesInvalidJSON(t *testing.T) {
	_, err := providerLinks([]byte(`not json`))
	require.Error(t, err)
}

// The dump writes prices the way a person edits them, and apply reads them
// back to the same exact terms.
func TestCatalogDumpIsReadable(t *testing.T) {
	source := []byte(`schema_version: 1
products:
  premium:
    display_name: Premium
    prices:
      monthly: {currency: USD, unit_amount: 9990000, access_duration_hours: 720, billing_interval_hours: 720, trial_unit_amount: 0, trial_duration_hours: 24}
      lifetime: {currency: JPY, unit_amount: 5000000, access_duration_hours: null, billing_interval_hours: null}
      gas: {currency: SOL, unit_amount: 1500000000, access_duration_hours: 168}
`)
	manifest, err := catalog.ParseApplicationYAML(source)
	require.NoError(t, err)
	raw, err := readableCatalogYAML(manifest)
	require.NoError(t, err)
	for _, line := range []string{"amount: 9.99 USD", "billing_interval: 30 days", "trial_duration: 1 day", "amount: 500 JPY", "access_duration: null", "amount: 1.5 SOL", "access_duration: 1 week"} {
		require.Contains(t, string(raw), line)
	}
	for _, numeric := range []string{"unit_amount: \"9990000\"", "_hours", "currency:"} {
		require.NotContains(t, string(raw), numeric)
	}
	back, err := catalog.ParseApplicationYAML(raw)
	require.NoError(t, err)
	require.Equal(t, manifest, back)
}
