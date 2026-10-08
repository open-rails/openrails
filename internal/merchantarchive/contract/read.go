package contract

import (
	"fmt"
	"io"

	"github.com/open-rails/openrails/internal/archivewire"
)

// Read applies the current billing schema and value contracts to the shared
// wire reader. Callers must roll back writes on any error, including a footer
// failure or missing table discovered after the final row.
func Read(src io.Reader, header func(archivewire.Header) error, row func(Profile, []*string) error) (archivewire.Info, error) {
	table := -1
	info, err := archivewire.Read(src, header, func(r archivewire.Record) error {
		if r.Kind == "table" {
			table++
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
