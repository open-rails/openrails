package contract

import (
	"fmt"
	"io"
	"strconv"

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
	info, err := archivewire.Read(src, header, func(r archivewire.Record) error {
		if r.Kind == "table" {
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
		// Revisions were inserted after merchant_id; the later credit columns
		// are appended. Older archives retain their identities and explicitly
		// promise no credit. The wire reader verifies the original digest.
		if (p.Name == "prices" || p.Name == "products") && len(r.Values) > 1 && r.Values[1] != nil && uuidPattern.MatchString(*r.Values[1]) {
			values := make([]*string, len(r.Values)+1)
			values[0] = r.Values[0]
			copy(values[2:], r.Values[1:])
			r.Values = values
		}
		if (p.Name == "prices" || p.Name == "products" || p.Name == "payments") && len(r.Values) == len(p.Columns)-1 {
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

		if err := ValidateValues(p, r.Values); err != nil {
			return err
		}
		if p.Name == "prices" {
			if id := value(p, r.Values, "id"); id != nil {
				priceAccess[*id] = value(p, r.Values, "access_duration_hours")
			}
		}
		if row != nil {
			return row(p, r.Values)
		}
		return nil
	})
	if err == nil && table != len(Profiles)-1 {
		err = fmt.Errorf("archive tables incomplete")
	}
	return ReadInfo{Info: info, LegacyDurations: legacyDurations}, err
}
