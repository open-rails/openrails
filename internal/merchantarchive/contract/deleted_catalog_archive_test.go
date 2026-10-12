package contract

import (
	"bytes"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

// An archive exported before migration 68 has products and prices without
// deleted_at: they read as never deleted. Its products, and those of an
// archive exported before migration 88, read as requiring no entitlements.
func TestReadArchiveCatalogBeforeDeletion(t *testing.T) {
	const product = "10000000-0000-0000-0000-000000000021"
	productsProfile, productRow := row(t, "products", map[string]string{"merchant_id": testMerchant, "revision": "1", "id": product, "key": "video",
		"display_name": "Video", "tier_rank": "0", "archived": "false", "created_at": "2026-09-01 00:00:00+00", "updated_at": "2026-09-01 00:00:00+00",
		"required_entitlements": "{}"})
	pricesProfile, priceRow := row(t, "prices", map[string]string{"merchant_id": testMerchant, "revision": "0", "id": "10000000-0000-0000-0000-000000000022",
		"product_id": product, "amount": "1000000", "currency": "USD", "archived": "false", "key": "buy",
		"created_at": "2026-09-01 00:00:00+00", "updated_at": "2026-09-01 00:00:00+00"})
	columns := productsProfile.Columns
	require.Equal(t, []string{"deleted_at", "required_entitlements"}, []string{columns[len(columns)-2].Name, columns[len(columns)-1].Name})
	require.Equal(t, "deleted_at", pricesProfile.Columns[len(pricesProfile.Columns)-1].Name)

	for name, cut := range map[string]struct{ products, prices int }{"before migration 68": {2, 1}, "before migration 88": {1, 0}} {
		var artifact bytes.Buffer
		writer, err := archivewire.NewWriter(&artifact, testMerchant)
		require.NoError(t, err)
		for _, p := range Profiles {
			require.NoError(t, writer.Table(p.Name))
			switch p.Name {
			case "products":
				require.NoError(t, writer.Row(productRow[:len(productRow)-cut.products]))
			case "prices":
				require.NoError(t, writer.Row(priceRow[:len(priceRow)-cut.prices]))
			}
		}
		require.NoError(t, writer.Close())

		read := map[string][]*string{}
		_, err = Read(bytes.NewReader(artifact.Bytes()), nil, func(p Profile, values []*string) error {
			if p.Name == "products" || p.Name == "prices" {
				read[p.Name] = values
			}
			return nil
		})
		require.NoError(t, err, name)
		require.Equal(t, productRow, read["products"], name)
		require.Equal(t, priceRow, read["prices"], name)
	}
}
