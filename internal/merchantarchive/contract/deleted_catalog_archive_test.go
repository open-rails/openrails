package contract

import (
	"bytes"
	"slices"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

// An archive exported before migration 68 has products without deleted_at:
// they read as never deleted. Its products, and those of an archive exported
// before migration 88, read as requiring no entitlements. Its prices predate
// explicit ownership and are refused with the restore path.
func TestReadArchiveCatalogBeforeDeletion(t *testing.T) {
	const product = "10000000-0000-0000-0000-000000000021"
	productsProfile, productRow := row(t, "products", map[string]string{"merchant_id": testMerchant, "revision": "1", "id": product, "key": "video",
		"display_name": "Video", "tier_rank": "0", "archived": "false", "created_at": "2026-09-01 00:00:00+00", "updated_at": "2026-09-01 00:00:00+00",
		"required_entitlements": "{}"})
	pricesProfile, priceRow := row(t, "prices", map[string]string{"merchant_id": testMerchant, "revision": "0", "id": "10000000-0000-0000-0000-000000000022",
		"product_id": product, "amount": "1000000", "currency": "USD", "archived": "false", "key": "buy",
		"created_at": "2026-09-01 00:00:00+00", "updated_at": "2026-09-01 00:00:00+00", "provider_owned": "false", "provider_access_default": "false"})
	columns := productsProfile.Columns
	require.Equal(t, []string{"deleted_at", "required_entitlements"}, []string{columns[len(columns)-2].Name, columns[len(columns)-1].Name})

	write := func(products, prices []*string) []byte {
		var artifact bytes.Buffer
		writer, err := archivewire.NewWriter(&artifact, testMerchant)
		require.NoError(t, err)
		for _, p := range Profiles {
			require.NoError(t, writer.Table(p.Name))
			switch p.Name {
			case "products":
				require.NoError(t, writer.Row(products))
			case "prices":
				require.NoError(t, writer.Row(prices))
			}
		}
		require.NoError(t, writer.Close())
		return artifact.Bytes()
	}
	for name, cut := range map[string]int{"before migration 68": 2, "before migration 88": 1} {
		read := map[string][]*string{}
		_, err := Read(bytes.NewReader(write(productRow[:len(productRow)-cut], priceRow)), nil, func(p Profile, values []*string) error {
			if p.Name == "products" || p.Name == "prices" {
				read[p.Name] = values
			}
			return nil
		})
		require.NoError(t, err, name)
		require.Equal(t, productRow, read["products"], name)
		require.Equal(t, priceRow, read["prices"], name)
	}

	owned := slices.IndexFunc(pricesProfile.Columns, func(c Column) bool { return c.Name == "provider_owned" })
	require.Positive(t, owned)
	_, err := Read(bytes.NewReader(write(productRow, priceRow[:owned])), nil, nil)
	require.ErrorContains(t, err, "restore using OpenRails v0.255.0, then upgrade")
}
