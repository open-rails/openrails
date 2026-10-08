package merchantarchive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/goccy/go-yaml"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/configdocument"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

// CatalogSnapshotMaxBytes bounds the complete YAML artifact before any writes.
const CatalogSnapshotMaxBytes = 64 << 20

// CatalogSnapshot preserves persisted catalog state, not an ApplyCatalog request.
// SQL NULL columns are omitted; JSON null remains an explicit YAML null. Monetary
// and other bigint scalars use decimal strings, avoiding client rounding.
type CatalogSnapshot struct {
	Kind            string                                  `json:"kind"`
	SchemaVersion   int                                     `json:"schema_version"`
	MerchantID      string                                  `json:"merchant_id"`
	CatalogRevision int64                                   `json:"catalog_revision,string"`
	Dependencies    CatalogDependencies                     `json:"dependencies"`
	Tables          map[string][]map[string]json.RawMessage `json:"tables"`
	SHA256          string                                  `json:"sha256,omitempty"`
}
type CatalogDependencies struct {
	PSPs      []CatalogPSPIdentity `json:"psps"`
	Customers []string             `json:"customers"`
}
type CatalogPSPIdentity struct {
	ID          string `json:"id"`
	Rail        string `json:"rail"`
	Environment string `json:"environment"`
	AccountID   string `json:"account_id"`
}

// The order is also the foreign-key insertion order. These are all eight
// persisted catalog tables; product/rate-card state prior to updates was never
// retained and cannot be reconstructed by an export.
var catalogProfiles = func() []contract.Profile {
	names := []string{"catalog_meters", "products", "prices", "price_key_movements", "price_psp_bindings", "catalog_rate_cards", "catalog_applications"}
	out := make([]contract.Profile, 0, len(names)+1)
	for _, name := range names {
		for _, p := range contract.Profiles {
			if p.Name == name {
				out = append(out, p)
				break
			}
		}
	}
	return append(out, contract.Profile{Name: "product_archive_operations", Columns: []contract.Column{
		{Name: "merchant_id", Type: "uuid"}, {Name: "id", Type: "uuid"}, {Name: "idempotency_key", Type: "text"}, {Name: "request_sha256", Type: "bytea"}, {Name: "product_id", Type: "uuid"}, {Name: "purchase_action", Type: "text"}, {Name: "purchase_window_starts_at", Type: "timestamp with time zone"}, {Name: "reason", Type: "text"}, {Name: "created_at", Type: "timestamp with time zone"},
	}})
}()

func catalogRow(p contract.Profile, values []*string) (map[string]json.RawMessage, error) {
	row := make(map[string]json.RawMessage, len(values))
	for i, c := range p.Columns {
		if values[i] == nil {
			continue
		}
		value := *values[i]
		switch c.Type {
		case "jsonb", "boolean", "integer", "smallint":
			row[c.Name] = json.RawMessage(value)
		default:
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			row[c.Name] = raw
		}
	}
	return row, nil
}
func catalogValues(p contract.Profile, row map[string]json.RawMessage, merchantID string) ([]*string, error) {
	if len(row) > len(p.Columns) {
		return nil, fmt.Errorf("unknown catalog columns in %s", p.Name)
	}
	values := make([]*string, len(p.Columns))
	known := map[string]bool{}
	for i, c := range p.Columns {
		known[c.Name] = true
		raw, ok := row[c.Name]
		if !ok {
			continue
		}
		var value string
		switch c.Type {
		case "jsonb":
			value = string(raw)
		case "boolean", "integer", "smallint":
			value = string(raw)
		default:
			if err := json.Unmarshal(raw, &value); err != nil || bytes.Equal(raw, []byte("null")) {
				return nil, fmt.Errorf("invalid scalar in %s.%s", p.Name, c.Name)
			}
		}
		values[i] = &value
	}
	for name := range row {
		if !known[name] {
			return nil, fmt.Errorf("unknown catalog column %s.%s", p.Name, name)
		}
	}
	if p.Name == "products" || p.Name == "prices" {
		if values[1] == nil {
			return nil, fmt.Errorf("catalog snapshot %s requires its original revision", p.Name)
		}
	}
	if values[0] == nil || *values[0] != merchantID {
		return nil, fmt.Errorf("catalog row merchant mismatch")
	}
	if err := contract.ValidateValues(p, values); err != nil {
		return nil, err
	}
	return values, nil
}

func catalogDigest(document CatalogSnapshot) (string, error) {
	document.SHA256 = ""
	raw, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	// Canonicalize JSON nested in RawMessage too: YAML key order and whitespace
	// never affect artifact identity, and no number passes through float64.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var tree any
	if err := d.Decode(&tree); err != nil {
		return "", err
	}
	raw, err = json.Marshal(tree)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func validateCatalogSnapshot(document CatalogSnapshot) (int64, error) {
	mid, err := billing.ParseMerchantID(document.MerchantID)
	if err != nil || mid.IsZero() || mid.String() != document.MerchantID || document.Kind != "catalog_snapshot" || document.SchemaVersion != 1 || document.CatalogRevision < 0 {
		return 0, fmt.Errorf("invalid catalog snapshot header")
	}
	if len(document.Tables) != len(catalogProfiles) {
		return 0, fmt.Errorf("catalog snapshot requires every catalog table")
	}
	var count int64
	for _, p := range catalogProfiles {
		rows, ok := document.Tables[p.Name]
		if !ok || rows == nil {
			return 0, fmt.Errorf("missing catalog table %s", p.Name)
		}
		for _, row := range rows {
			if _, err := catalogValues(p, row, document.MerchantID); err != nil {
				return 0, err
			}
			count++
		}
	}
	digest, err := catalogDigest(document)
	if err != nil {
		return 0, err
	}
	if document.SHA256 != digest {
		return 0, fmt.Errorf("catalog snapshot digest mismatch")
	}
	return count, nil
}

// WriteCatalogSnapshot seals a complete document and writes human-readable YAML.
func WriteCatalogSnapshot(out io.Writer, document CatalogSnapshot) error {
	var err error
	document.SHA256, err = catalogDigest(document)
	if err != nil {
		return err
	}
	if _, err = validateCatalogSnapshot(document); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	raw, err := yaml.JSONToYAML(encoded)
	if err != nil {
		return err
	}
	// Apply the same structural bounds as the importer before writing anything.
	if _, err = configdocument.YAMLToJSON(raw, CatalogSnapshotMaxBytes); err != nil {
		return err
	}
	_, err = out.Write(raw)
	return err
}

func readCatalogSnapshot(in io.Reader) (CatalogSnapshot, int64, error) {
	var document CatalogSnapshot
	raw, err := io.ReadAll(io.LimitReader(in, CatalogSnapshotMaxBytes+1))
	if err != nil {
		return document, 0, err
	}
	raw, err = configdocument.YAMLToJSON(raw, CatalogSnapshotMaxBytes)
	if err != nil {
		return document, 0, err
	}
	if err = configdocument.GuardJSON(raw, CatalogSnapshotMaxBytes); err != nil {
		return document, 0, err
	}
	// Envelope field names are exact; per-table row columns are checked below.
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return document, 0, err
	}
	for name := range envelope {
		switch name {
		case "kind", "schema_version", "merchant_id", "catalog_revision", "dependencies", "tables", "sha256":
		default:
			return document, 0, fmt.Errorf("unknown snapshot field %q", name)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&document); err != nil {
		return document, 0, err
	}
	count, err := validateCatalogSnapshot(document)
	return document, count, err
}
