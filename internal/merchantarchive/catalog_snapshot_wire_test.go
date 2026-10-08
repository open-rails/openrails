package merchantarchive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/stretchr/testify/require"
)

const snapshotTestMerchant = "10000000-0000-0000-0000-000000000001"

func snapshotWireFixture(t *testing.T) CatalogSnapshot {
	t.Helper()
	document := CatalogSnapshot{
		Kind: "catalog_snapshot", SchemaVersion: 1, MerchantID: snapshotTestMerchant,
		Dependencies: CatalogDependencies{PSPs: []CatalogPSPIdentity{}, Customers: []string{}},
		Tables:       map[string][]map[string]json.RawMessage{},
	}
	for _, table := range []string{"products", "prices", "price_key_movements", "price_psp_bindings", "catalog_meters", "catalog_rate_cards", "catalog_applications", "product_archive_operations"} {
		document.Tables[table] = []map[string]json.RawMessage{}
	}
	return document
}

func snapshotWireProfile(t *testing.T, table string) contract.Profile {
	t.Helper()
	for _, profile := range catalogProfiles {
		if profile.Name == table {
			return profile
		}
	}
	t.Fatalf("missing catalog profile %s", table)
	return contract.Profile{}
}

func snapshotWireProduct(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	return snapshotWireRow(t, "products", map[string]string{
		"merchant_id": snapshotTestMerchant, "id": "10000000-0000-0000-0000-000000000002",
		"revision": "3", "key": "premium", "display_name": "Premium", "tier_rank": "0", "archived": "false",
		"created_at": "2026-10-07 12:00:00+00", "updated_at": "2026-10-07 12:00:00+00",
	})
}

func snapshotWireRow(t *testing.T, table string, fields map[string]string) map[string]json.RawMessage {
	t.Helper()
	profile := snapshotWireProfile(t, table)
	values := make([]*string, len(profile.Columns))
	for i, column := range profile.Columns {
		if value, ok := fields[column.Name]; ok {
			values[i] = &value
		}
	}
	row, err := catalogRow(profile, values)
	require.NoError(t, err)
	return row
}

// Seal without the writer's validation so malformed input exercises the reader.
func snapshotWireUncheckedYAML(t *testing.T, document CatalogSnapshot) []byte {
	t.Helper()
	var err error
	document.SHA256, err = catalogDigest(document)
	require.NoError(t, err)
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	raw, err = yaml.JSONToYAML(raw)
	require.NoError(t, err)
	return raw
}

func TestCatalogSnapshotWireRoundTripPreservesNullAndExactIntegers(t *testing.T) {
	document := snapshotWireFixture(t)
	document.CatalogRevision = 9007199254740993
	product := snapshotWireProduct(t)
	product["entitlements_spec"] = json.RawMessage("null")
	// credit_grant is SQL NULL (absent), while entitlements_spec is JSON null.
	document.Tables["products"] = []map[string]json.RawMessage{product}
	document.Tables["prices"] = []map[string]json.RawMessage{snapshotWireRow(t, "prices", map[string]string{
		"merchant_id": snapshotTestMerchant, "id": "10000000-0000-0000-0000-000000000003",
		"product_id": "10000000-0000-0000-0000-000000000002", "revision": "7", "key": "purchase",
		"amount": "9007199254740993", "currency": "USD", "archived": "true", "auto_renew": "false",
		"created_at": "2026-10-07 12:00:00+00", "updated_at": "2026-10-07 12:00:00+00",
	})}
	document.Tables["catalog_rate_cards"] = []map[string]json.RawMessage{snapshotWireRow(t, "catalog_rate_cards", map[string]string{
		"merchant_id": snapshotTestMerchant, "id": "10000000-0000-0000-0000-000000000004",
		"product_id": "10000000-0000-0000-0000-000000000002", "ordinal": "1", "payment_term": "in_arrears", "filter": "{}",
		"price":      `{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":"9007199254740993","divide_by":9007199254740993}}`,
		"created_at": "2026-10-07 12:00:00+00", "updated_at": "2026-10-07 12:00:00+00",
	})}
	var encoded bytes.Buffer
	require.NoError(t, WriteCatalogSnapshot(&encoded, document))
	restored, count, err := readCatalogSnapshot(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)
	require.EqualValues(t, 3, count)
	require.Equal(t, document.CatalogRevision, restored.CatalogRevision)
	require.JSONEq(t, `"9007199254740993"`, string(restored.Tables["prices"][0]["amount"]))
	var rate struct {
		PerUnit struct {
			UnitAmount string      `json:"unit_amount"`
			DivideBy   json.Number `json:"divide_by"`
		} `json:"per_unit"`
	}
	require.NoError(t, json.Unmarshal(restored.Tables["catalog_rate_cards"][0]["price"], &rate))
	require.Equal(t, "9007199254740993", rate.PerUnit.UnitAmount)
	require.Equal(t, "9007199254740993", rate.PerUnit.DivideBy.String())
	profile := snapshotWireProfile(t, "products")
	values, err := catalogValues(profile, restored.Tables["products"][0], snapshotTestMerchant)
	require.NoError(t, err)
	for i, column := range profile.Columns {
		switch column.Name {
		case "entitlements_spec":
			require.NotNil(t, values[i])
			require.Equal(t, "null", *values[i])
		case "credit_grant":
			require.Nil(t, values[i])
		}
	}
}

func TestCatalogSnapshotWireRejectsAmbiguousOrChangedYAML(t *testing.T) {
	var encoded bytes.Buffer
	require.NoError(t, WriteCatalogSnapshot(&encoded, snapshotWireFixture(t)))
	valid := encoded.String()
	for name, raw := range map[string]string{
		"duplicate key":            valid + "kind: catalog_snapshot\n",
		"anchor and alias":         strings.Replace(strings.Replace(valid, "prices: []", "prices: &empty []", 1), "products: []", "products: *empty", 1),
		"multiple documents":       valid + "---\nkind: catalog_snapshot\n",
		"unknown field":            valid + "unrecognized: true\n",
		"unknown dependency field": strings.Replace(valid, "dependencies:", "dependencies:\n  unrecognized: true", 1),
		"changed content":          strings.Replace(valid, snapshotTestMerchant, "10000000-0000-0000-0000-000000000099", 1),
		"invalid schema":           strings.Replace(valid, "schema_version: 1", "schema_version: 2", 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, valid, raw)
			_, _, err := readCatalogSnapshot(strings.NewReader(raw))
			require.Error(t, err)
		})
	}
}

func TestCatalogSnapshotWireRejectsMissingIdentityAndUnknownColumns(t *testing.T) {
	for _, table := range []string{"products", "prices"} {
		t.Run(table+" revision is required", func(t *testing.T) {
			document := snapshotWireFixture(t)
			row := map[string]json.RawMessage{
				"merchant_id": json.RawMessage(`"` + snapshotTestMerchant + `"`),
				"id":          json.RawMessage(`"10000000-0000-0000-0000-000000000002"`),
			}
			document.Tables[table] = []map[string]json.RawMessage{row}
			_, _, err := readCatalogSnapshot(bytes.NewReader(snapshotWireUncheckedYAML(t, document)))
			require.ErrorContains(t, err, "revision")
		})
	}
	for name, mutate := range map[string]func(*CatalogSnapshot){
		"unknown column": func(document *CatalogSnapshot) {
			document.Tables["products"][0]["unexpected"] = json.RawMessage("true")
		},
		"column casing": func(document *CatalogSnapshot) { document.Tables["products"][0]["Revision"] = json.RawMessage(`"3"`) },
		"missing table": func(document *CatalogSnapshot) { delete(document.Tables, "prices") },
		"unknown table": func(document *CatalogSnapshot) { document.Tables["payments"] = []map[string]json.RawMessage{} },
		"cross merchant row": func(document *CatalogSnapshot) {
			document.Tables["products"][0]["merchant_id"] = json.RawMessage(`"10000000-0000-0000-0000-000000000099"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			document := snapshotWireFixture(t)
			document.Tables["products"] = []map[string]json.RawMessage{snapshotWireProduct(t)}
			mutate(&document)
			_, _, err := readCatalogSnapshot(bytes.NewReader(snapshotWireUncheckedYAML(t, document)))
			require.Error(t, err)
		})
	}
}
