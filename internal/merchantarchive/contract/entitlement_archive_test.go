package contract

import (
	"bytes"
	"io"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/stretchr/testify/require"
)

func TestReadHistoricalEntitlementMapsKeepsAcceptedHours(t *testing.T) {
	const priceID = "10000000-0000-0000-0000-000000000003"
	const legacy = `{"service:z":null,"article:42":48,"permanent":0}`
	product := legacyProduct(map[string]string{"merchant_id": testMerchant, "revision": "0", "id": "10000000-0000-0000-0000-000000000002", "created_at": "2026-09-01 00:00:00+00", "entitlements": legacy})
	_, price := row(t, "prices", map[string]string{"merchant_id": testMerchant, "id": priceID, "revision": "0"})
	payment := legacyRow(t, LegacyPayments, map[string]string{
		"merchant_id": testMerchant, "price_id": priceID, "entitlements_snapshot": legacy,
		"metadata": `{"legacy_entitlement_hours":{"application-key":24}}`,
	})
	// The private historical-evidence column did not exist in this archive.
	payment = payment[:len(payment)-1]
	var artifact bytes.Buffer
	writer, err := archivewire.NewVersionWriter(&artifact, 1, testMerchant)
	require.NoError(t, err)
	for _, p := range ProfilesFor(1) {
		if p.Name == "invoice_collection_cadence" || p.Name == "product_entitlements" {
			continue
		} // Not present in the historical format.
		require.NoError(t, writer.Table(p.Name))
		switch p.Name {
		case "products":
			require.NoError(t, writer.Row(product))
		case "prices":
			require.NoError(t, writer.Row(price))
		case "payments":
			require.NoError(t, writer.Row(payment))
		}
	}
	require.NoError(t, writer.Close())
	wire, err := archivewire.CopyVerified(io.Discard, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	var keys []string
	info, err := Read(bytes.NewReader(artifact.Bytes()), nil, func(p Profile, values []*string) error {
		switch p.Name {
		case "products":
			require.Len(t, values, len(p.Columns))
		case "product_entitlements":
			require.Equal(t, "10000000-0000-0000-0000-000000000002", *value(p, values, "product_id"))
			require.Equal(t, PrehistoricKeyAddedAt, *value(p, values, "added_at"), "valid from before key history")
			require.Nil(t, value(p, values, "removed_at"))
			keys = append(keys, *value(p, values, "entitlement"))
		case "payments":
			require.Len(t, values, len(namedProfile("payments").Columns), "a payment keeps no keys")
			require.Equal(t, `{"legacy_entitlement_hours":{"application-key":24}}`, *value(p, values, "metadata"), "application metadata is not accepted duration evidence")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"article:42", "permanent", "service:z"}, keys)
	require.Equal(t, wire.Digest, info.Digest, "normalization preserves the original archive identity")
}

// legacyProduct fills a products row of an archive preceding product_entitlements.
func legacyProduct(fields map[string]string) []*string {
	values := make([]*string, len(LegacyProducts.Columns))
	for i, c := range LegacyProducts.Columns {
		if v, ok := fields[c.Name]; ok {
			values[i] = &v
		}
	}
	return values
}

func TestStoredEntitlementListsAndPrivateHoursAreValidated(t *testing.T) {
	for _, names := range []string{`["same","same"]`, `[""]`, `["  "]`, `{"old":null}`} {
		require.Error(t, ValidateValues(LegacyProducts, legacyProduct(map[string]string{"entitlements": names})), names)
	}
	for _, hours := range []string{`{"a":0}`, `{"a":-1}`, `{"a":2562048}`, `{"a":1.5}`, `{"other":1}`} {
		values := legacyRow(t, LegacyPayments, map[string]string{"entitlements_snapshot": `["a"]`, "legacy_entitlement_hours": hours})
		require.Error(t, ValidateValues(LegacyPayments, values), hours)
	}
}
