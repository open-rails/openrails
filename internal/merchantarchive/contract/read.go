package contract

import (
	"fmt"
	"io"
	"slices"

	"github.com/open-rails/openrails/internal/archivewire"
)

// Read applies the current billing schema and value contracts to the shared
// wire reader: every table in Profiles order, then the event store section.
// Callers must roll back writes on any error, including a footer failure or
// missing table discovered after the final row.
func Read(src io.Reader, header func(archivewire.Header) error, row func(Profile, []*string) error) (archivewire.Info, error) {
	table := -1
	// The event store section follows every table: store is its table.
	store := -1
	info, err := archivewire.Read(src, header, func(r archivewire.Record) error {
		if r.Kind == "table" {
			if i := slices.IndexFunc(EventStoreProfiles, func(p Profile) bool { return p.Name == r.Table }); i >= 0 {
				if table != len(Profiles)-1 || i <= store {
					return fmt.Errorf("invalid archive table order")
				}
				store = i
				return nil
			}
			table++
			if store >= 0 || table >= len(Profiles) || r.Table != Profiles[table].Name {
				return fmt.Errorf("invalid archive table order")
			}
			return nil
		}
		p := Profiles[table]
		if store >= 0 {
			p = EventStoreProfiles[store]
		}
		if err := ValidateValues(p, r.Values); err != nil {
			return err
		}
		if row != nil {
			return row(p, r.Values)
		}
		return nil
	})
	if err == nil && table != len(Profiles)-1 {
		err = fmt.Errorf("archive tables incomplete")
	}
	return info, err
}

// ClearMeteredPaymentTerm drops the payment term a metered rate card carried
// before rating at ingest: only a flat fee states one since migration 66.
func ClearMeteredPaymentTerm(p Profile, values []*string) {
	if len(values) != len(p.Columns) || value(p, values, "meter_key") == nil {
		return
	}
	for i, c := range p.Columns {
		if c.Name == "payment_term" {
			values[i] = nil
		}
	}
}
