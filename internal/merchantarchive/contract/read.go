package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/archivewire"
)

// ReadInfo keeps the original wire identity while identifying historical rows
// whose derived entitlement projection needs the same repair as migration 8.
type ReadInfo struct {
	archivewire.Info
	LegacyDurations bool
}

// Read applies the current billing schema and value contracts to the shared
// wire reader. Callers must roll back writes on any error, including a footer
// failure or missing table discovered after the final row.
func Read(src io.Reader, header func(archivewire.Header) error, row func(Profile, []*string) error) (ReadInfo, error) {
	table := -1
	legacyDurations := false
	priceAccess := map[string]*string{}
	// Products rows are held until the next table shows their shape: an
	// archive with product_entitlements carries keys there; an older one
	// carries them on each product.
	var products [][]*string
	emit := func(p Profile, values []*string) error {
		if err := ValidateValues(p, values); err != nil {
			return err
		}
		if row != nil {
			return row(p, values)
		}
		return nil
	}
	info, err := archivewire.Read(src, header, func(r archivewire.Record) error {
		if r.Kind == "table" {
			if table >= 0 && Profiles[table].Name == "products" {
				current := r.Table == "product_entitlements"
				if err := flushProducts(products, current, emit); err != nil {
					return err
				}
				products = nil
				if !current {
					table++ // product_entitlements, synthesized from the products
				}
			}
			table++
			// Archives preceding invoice cadence have no completed scan to retain.
			if table < len(Profiles) && Profiles[table].Name == "invoice_collection_cadence" && r.Table == "invoices" {
				table++
			}
			if table >= len(Profiles) || r.Table != Profiles[table].Name {
				return fmt.Errorf("invalid archive table order")
			}
			return nil
		}
		p := Profiles[table]
		if p.Name == "products" {
			products = append(products, r.Values)
			return nil
		}
		// Revisions were inserted after merchant_id; the later credit columns
		// are appended. Older archives retain their identities and explicitly
		// promise no credit. The wire reader verifies the original digest.
		if p.Name == "prices" && len(r.Values) > 1 && r.Values[1] != nil && uuidPattern.MatchString(*r.Values[1]) {
			r.Values = insertRevision(r.Values)
		}
		if (p.Name == "prices" || p.Name == "payments") && len(r.Values) == len(p.Columns)-1 {
			r.Values = append(r.Values, nil)
		}
		if p.Name == "payments" && len(r.Values) == len(p.Columns)-2 {
			r.Values = append(r.Values, nil, nil) // Before purchased credits and private legacy duration evidence.
		}
		if p.Name == "prices" && len(r.Values) == len(p.Columns) {
			for i, c := range p.Columns {
				if c.Name != "billing_interval_hours" || r.Values[i] == nil {
					continue
				}
				switch *r.Values[i] {
				case "true":
					access := value(p, r.Values, "access_duration_hours")
					if access == nil {
						return fmt.Errorf("legacy recurring price has no billing duration")
					}
					hours, err := strconv.Atoi(*access)
					if err != nil || hours <= 0 {
						return fmt.Errorf("legacy recurring price has invalid billing duration")
					}
					r.Values[i], legacyDurations = access, true
				case "false":
					r.Values[i], legacyDurations = nil, true
				}
			}
		}
		if p.Name == "subscriptions" && len(r.Values) == len(p.Columns)-1 {
			// Prices precede subscriptions in the verified archive. Older rows
			// accepted access from their immutable price, as migration 8 does.
			var access *string
			if r.Values[2] != nil {
				var ok bool
				access, ok = priceAccess[*r.Values[2]]
				if !ok {
					return fmt.Errorf("legacy subscription price is missing")
				}
			}
			for i, c := range p.Columns {
				if c.Name == "access_duration_hours_snapshot" {
					r.Values = append(r.Values, nil)
					copy(r.Values[i+1:], r.Values[i:])
					r.Values[i] = access
					break
				}
			}
			legacyDurations = true
		}
		if len(r.Values) == len(p.Columns) {
			if err := normalizeEntitlementRow(p, r.Values, priceAccess); err != nil {
				return err
			}
		}
		if p.Name == "prices" {
			if err := ValidateValues(p, r.Values); err != nil {
				return err
			}
			if id := value(p, r.Values, "id"); id != nil {
				priceAccess[*id] = value(p, r.Values, "access_duration_hours")
			}
			if row != nil {
				return row(p, r.Values)
			}
			return nil
		}
		return emit(p, r.Values)
	})
	if err == nil && table != len(Profiles)-1 {
		err = fmt.Errorf("archive tables incomplete")
	}
	return ReadInfo{Info: info, LegacyDurations: legacyDurations}, err
}

func insertRevision(values []*string) []*string {
	out := make([]*string, len(values)+1)
	out[0] = values[0]
	copy(out[2:], values[1:])
	return out
}

// flushProducts emits held products rows. Rows of an archive preceding
// product_entitlements carry their keys, which become product_entitlements
// rows valid from before key history, as migration 15 converts them.
func flushProducts(rows [][]*string, current bool, emit func(Profile, []*string) error) error {
	products := namedProfile("products")
	if current {
		for _, values := range rows {
			if err := emit(products, values); err != nil {
				return err
			}
		}
		return nil
	}
	legacy := LegacyProducts
	var keys [][]*string
	for _, values := range rows {
		if len(values) > 1 && values[1] != nil && uuidPattern.MatchString(*values[1]) {
			values = insertRevision(values)
		}
		if len(values) == len(legacy.Columns)-1 {
			values = append(values, nil)
		}
		if len(values) != len(legacy.Columns) {
			return fmt.Errorf("invalid row width for products")
		}
		if err := normalizeEntitlementRow(legacy, values, nil); err != nil {
			return err
		}
		if err := ValidateValues(legacy, values); err != nil {
			return err
		}
		var names []string
		if raw := value(legacy, values, "entitlements"); raw != nil {
			if err := json.Unmarshal([]byte(*raw), &names); err != nil {
				return err
			}
		}
		current := make([]*string, 0, len(products.Columns))
		for i, c := range legacy.Columns {
			if c.Name != "entitlements" {
				current = append(current, values[i])
			}
		}
		if err := emit(products, current); err != nil {
			return err
		}
		merchant, id := value(legacy, values, "merchant_id"), value(legacy, values, "id")
		if merchant == nil || id == nil {
			return fmt.Errorf("legacy product lacks its identity")
		}
		for _, name := range names {
			key := uuid.NewSHA1(uuid.NameSpaceOID, []byte("openrails.product_entitlement\x00"+*id+"\x00"+name)).String()
			keys = append(keys, []*string{merchant, &key, id, &name, new(PrehistoricKeyAddedAt), nil, new("migration"), nil})
		}
	}
	entitlements := namedProfile("product_entitlements")
	for _, values := range keys {
		if err := emit(entitlements, values); err != nil {
			return err
		}
	}
	return nil
}

// PrehistoricKeyAddedAt is when a key that predates key history was added:
// the start of time, so older purchases derive it.
const PrehistoricKeyAddedAt = "0001-01-01 00:00:00+00"

func namedProfile(name string) Profile {
	for _, p := range Profiles {
		if p.Name == name {
			return p
		}
	}
	panic("archive profile " + name + " is missing")
}
