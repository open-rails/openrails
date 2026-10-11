package contract

import (
	"bytes"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

// An archive exported before migration 66 carries each rate card's customer
// and revision. A product's card reads in the current shape; a customer's
// override is skipped, as the migration deletes it.
func TestReadArchiveRateCardsBeforeOverridesWereDeleted(t *testing.T) {
	_, card := row(t, "catalog_rate_cards", map[string]string{"merchant_id": testMerchant, "id": "10000000-0000-0000-0000-000000000011",
		"product_id": "10000000-0000-0000-0000-000000000012", "ordinal": "1", "meter_key": "requests", "payment_term": "in_arrears",
		"filter": "{}", "price": `{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":"1000"}}`,
		"created_at": "2026-09-01 00:00:00+00", "updated_at": "2026-09-01 00:00:00+00"})
	customer, revision := "10000000-0000-0000-0000-000000000013", "1"
	productCard := append(append([]*string{}, card...), nil, &revision)
	override := append(append([]*string{}, card...), &customer, &revision)
	override[2] = nil // an override names no product

	var artifact bytes.Buffer
	writer, err := archivewire.NewWriter(&artifact, testMerchant)
	require.NoError(t, err)
	for _, p := range Profiles {
		require.NoError(t, writer.Table(p.Name))
		if p.Name == "catalog_rate_cards" {
			require.NoError(t, writer.Row(productCard))
			require.NoError(t, writer.Row(override))
		}
	}
	require.NoError(t, writer.Close())

	var read [][]*string
	_, err = Read(bytes.NewReader(artifact.Bytes()), nil, func(p Profile, values []*string) error {
		if p.Name == "catalog_rate_cards" {
			read = append(read, values)
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, [][]*string{card}, read)
}
