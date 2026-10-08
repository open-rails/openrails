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
	_, product := row(t, "products", map[string]string{"merchant_id": testMerchant, "revision": "0", "entitlements": legacy})
	_, price := row(t, "prices", map[string]string{"merchant_id": testMerchant, "id": priceID, "revision": "0"})
	_, payment := row(t, "payments", map[string]string{
		"merchant_id": testMerchant, "price_id": priceID, "entitlements_snapshot": legacy,
		"metadata": `{"legacy_entitlement_hours":{"application-key":24}}`,
	})
	// The private historical-evidence column did not exist in this archive.
	payment = payment[:len(payment)-1]
	var artifact bytes.Buffer
	writer, err := archivewire.NewWriter(&artifact, testMerchant)
	require.NoError(t, err)
	for _, p := range Profiles {
		if p.Name == "invoice_collection_cadence" {
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
	info, err := Read(bytes.NewReader(artifact.Bytes()), nil, func(p Profile, values []*string) error {
		switch p.Name {
		case "products":
			require.Equal(t, `["article:42","permanent","service:z"]`, *value(p, values, "entitlements"))
		case "payments":
			require.Equal(t, `["article:42","permanent","service:z"]`, *value(p, values, "entitlements_snapshot"))
			require.JSONEq(t, `{"article:42":48}`, *value(p, values, "legacy_entitlement_hours"))
			require.Equal(t, `{"legacy_entitlement_hours":{"application-key":24}}`, *value(p, values, "metadata"), "application metadata is not accepted duration evidence")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, wire.Digest, info.Digest, "normalization preserves the original archive identity")
}

func TestStoredEntitlementListsAndPrivateHoursAreValidated(t *testing.T) {
	for _, names := range []string{`["same","same"]`, `[""]`, `["  "]`, `{"old":null}`} {
		p, values := row(t, "products", map[string]string{"entitlements": names})
		require.Error(t, ValidateValues(p, values), names)
	}
	for _, hours := range []string{`{"a":0}`, `{"a":-1}`, `{"a":2562048}`, `{"a":1.5}`, `{"other":1}`} {
		p, values := row(t, "payments", map[string]string{"entitlements_snapshot": `["a"]`, "legacy_entitlement_hours": hours})
		require.Error(t, ValidateValues(p, values), hours)
	}
}
