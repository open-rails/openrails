package hosttools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
)

// All examples use the same single-merchant application contract as HTTP.
// The main example retains its metered matrix, monthly caps and allowances.
func TestExampleCatalogApplicationsParse(t *testing.T) {
	examples := map[string]int{"catalog.example.yaml": 6, "catalog.subscriptions.example.yaml": 3, "catalog.membership.example.yaml": 4}
	var metered *catalog.Application
	for name, count := range examples {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", name))
		require.NoError(t, err)
		application, err := catalog.ParseApplicationYAML(raw)
		require.NoError(t, err, name)
		require.Len(t, application.Products, count, name)
		require.NotEmpty(t, application.ApplicationID)
		require.NotNil(t, application.ExpectedRevision)
		require.False(t, application.Prune)
		if name == "catalog.example.yaml" {
			metered = application
		}
	}
	require.Len(t, metered.Meters, 8)
	products := map[string]catalog.ApplyProduct{}
	for _, product := range metered.Products {
		products[product.Key] = product
	}
	droplet := products["droplet"]
	require.False(t, droplet.TierGroup.Set, "usage products are not tier subscriptions")
	require.True(t, droplet.RateCards.Set)
	var matrix *catalog.RatePrice
	sawAllowance := false
	for _, product := range metered.Products {
		for _, card := range product.RateCards.Value {
			if card.Allowance != nil {
				sawAllowance = true
			}
		}
	}
	for i := range droplet.RateCards.Value {
		price := &droplet.RateCards.Value[i].Price
		if price.PerUnit != nil && price.PerUnit.Matrix != nil {
			matrix = price
		}
	}
	require.NotNil(t, matrix)
	model, ok := matrix.ChargeModelForCell("s-1vcpu-1gb")
	require.True(t, ok)
	cost, err := model.Rate(720 * 3600)
	require.NoError(t, err)
	require.Equal(t, int64(6000000), cost, "a full month retains the monthly cap")
	require.True(t, sawAllowance, "pooled egress allowance must survive the example conversion")
}

func TestCatalogApplicationFileCannotSelectAuthority(t *testing.T) {
	for _, field := range []string{"merchant: another-merchant", "merchants: []", "auth: {}", "catalogs: []"} {
		_, err := ApplyMerchantCatalog(context.Background(), CatalogApplyOptions{
			Merchant: "operator-selected",
			Manifest: []byte("schema_version: 1\napplication_id: invalid-authority\nexpected_revision: 0\n" + field + "\n"),
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown field")
	}
}
