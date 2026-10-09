//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

// legacyArchiveEdit rewrites one version 1 row; nil drops it.
type legacyArchiveEdit func(table string, values []*string) []*string

// legacyArchive rewrites a current archive as version 1 wrote it: per-key
// entitlement windows (one per key of each product-access window, projecting
// an entitlement grant) in place of product access, key snapshots on payments
// and subscriptions, and grants without free-grant attribution. edit adjusts
// each version 1 row; skip drops whole tables.
func legacyArchive(t *testing.T, current []byte, edit legacyArchiveEdit, skip ...string) []byte {
	t.Helper()
	currentProfiles := map[string]contract.Profile{}
	for _, p := range contract.Profiles {
		currentProfiles[p.Name] = p
	}
	legacy := map[string]contract.Profile{}
	for _, p := range contract.ProfilesFor(1) {
		legacy[p.Name] = p
	}
	skipped := map[string]bool{}
	for _, name := range skip {
		skipped[name] = true
	}
	field := func(p contract.Profile, values []*string, name string) *string {
		for i, c := range p.Columns {
			if c.Name == name {
				return values[i]
			}
		}
		return nil
	}
	keys := map[string][]string{}
	priceProducts := map[string]string{}
	accessGrants := map[string]bool{}
	snapshot := func(product *string) *string {
		if product == nil {
			return nil
		}
		raw, err := json.Marshal(append([]string{}, keys[*product]...))
		require.NoError(t, err)
		return new(string(raw))
	}
	var out bytes.Buffer
	var writer *archivewire.Writer
	table := ""
	write := func(values []*string) error {
		name := table
		if name == "product_access" {
			name = "entitlements"
		}
		if edit != nil {
			if values = edit(name, values); values == nil {
				return nil
			}
		}
		return writer.Row(values)
	}
	_, err := archivewire.Read(bytes.NewReader(current), func(h archivewire.Header) error {
		var err error
		writer, err = archivewire.NewVersionWriter(&out, 1, h.MerchantID, h.CatalogRevision)
		return err
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			table = record.Table
			if skipped[table] {
				return nil
			}
			if table == "product_access" {
				return writer.Table("entitlements")
			}
			return writer.Table(table)
		}
		from := currentProfiles[table]
		values := record.Values
		switch table {
		case "product_entitlements":
			if field(from, values, "removed_at") == nil {
				product := *field(from, values, "product_id")
				keys[product] = append(keys[product], *field(from, values, "entitlement"))
			}
		case "prices":
			priceProducts[*field(from, values, "id")] = *field(from, values, "product_id")
		case "grants":
			if *field(from, values, "kind") == "access" {
				accessGrants[*field(from, values, "id")] = true
			}
		}
		if skipped[table] {
			return nil
		}
		if table == "product_access" {
			product, grant := field(from, values, "product_id"), field(from, values, "grant_id")
			sourceType, sourceID := *field(from, values, "source_type"), field(from, values, "source_id")
			if sourceType == "grant" {
				sourceType, sourceID = "admin", grant
			}
			to := legacy["entitlements"]
			for _, key := range keys[*product] {
				row := make([]*string, len(to.Columns))
				for i, c := range to.Columns {
					switch c.Name {
					case "id":
						row[i] = new(uuid.NewSHA1(uuid.NameSpaceOID, []byte(*field(from, values, "id")+key)).String())
					case "entitlement":
						row[i] = new(key)
					case "source_type":
						row[i] = new(sourceType)
					case "source_id":
						row[i] = sourceID
					default:
						row[i] = field(from, values, c.Name)
					}
				}
				if err := write(row); err != nil {
					return err
				}
			}
			return nil
		}
		to, ok := legacy[table]
		if !ok || len(to.Columns) == len(from.Columns) && table != "grants" {
			return write(values)
		}
		row := make([]*string, len(to.Columns))
		for i, c := range to.Columns {
			row[i] = field(from, values, c.Name)
		}
		set := func(name string, v *string) {
			for i, c := range to.Columns {
				if c.Name == name {
					row[i] = v
				}
			}
		}
		switch table {
		case "payments":
			if price := field(from, values, "price_id"); price != nil {
				product := priceProducts[*price]
				set("entitlements_snapshot", snapshot(&product))
			}
		case "subscriptions":
			set("entitlements_snapshot", snapshot(field(from, values, "product_id")))
		case "grants":
			original := field(from, values, "supersedes_id")
			if *field(from, values, "kind") == "access" || original != nil && accessGrants[*original] {
				set("kind", new("entitlement"))
				if *field(from, values, "source_type") == "grant" {
					set("source_type", new("admin"))
				}
				if *field(from, values, "event") == "grant" {
					raw, err := json.Marshal(map[string][]string{"entitlements": append([]string{}, keys[*field(from, values, "product_id")]...)})
					require.NoError(t, err)
					set("spec_snapshot", new(string(raw)))
				}
			}
		}
		return write(row)
	})
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return out.Bytes()
}
