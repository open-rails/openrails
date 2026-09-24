package hosttools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/catalog"
)

// The shipped examples are the single-merchant application contract; the main
// example keeps its metered matrix, monthly cap and pooled allowance.
func TestExampleCatalogApplicationsParse(t *testing.T) {
	var metered *catalog.Application
	for name, products := range map[string]int{"catalog.example.yaml": 6, "catalog.subscriptions.example.yaml": 3, "catalog.membership.example.yaml": 4} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", name))
		require.NoError(t, err)
		application, err := catalog.ParseApplicationYAML(raw)
		require.NoError(t, err, name)
		require.Len(t, application.Products, products, name)
		require.NotEmpty(t, application.ApplicationID, name)
		require.NotNil(t, application.ExpectedRevision, name)
		require.False(t, application.Prune, "an example never deletes omitted items: %s", name)
		if name == "catalog.example.yaml" {
			metered = application
		}
	}
	require.Len(t, metered.Meters, 8)
	var matrix *catalog.RatePrice
	allowance := false
	for _, product := range metered.Products {
		for i := range product.RateCards.Value {
			card := &product.RateCards.Value[i]
			allowance = allowance || card.Allowance != nil
			if product.Key == "droplet" && card.Price.PerUnit != nil && card.Price.PerUnit.Matrix != nil {
				require.False(t, product.TierGroup.Set, "usage products are not tier subscriptions")
				matrix = &card.Price
			}
		}
	}
	require.True(t, allowance, "pooled egress allowance survives")
	require.NotNil(t, matrix)
	model, ok := matrix.ChargeModelForCell("s-1vcpu-1gb")
	require.True(t, ok)
	cost, err := model.Rate(720 * 3600)
	require.NoError(t, err)
	require.Equal(t, int64(6000000), cost, "a full month is capped at the monthly price")
}

// The operator selects the merchant; the document can never select authority.
func TestApplyMerchantCatalogPreconditions(t *testing.T) {
	const header = "schema_version: 1\napplication_id: invalid-authority\nexpected_revision: 0\n"
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
		"invalid":         {`not json`, nil},
	} {
		require.Equal(t, tc.want, providerLinks([]byte(tc.raw)), name)
	}
}
