package contract

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/archivewire"
)

// Read applies the current billing schema and value contracts to the shared
// wire reader. Callers must roll back writes on any error, including a footer
// failure or missing table discovered after the final row.
func Read(src io.Reader, header func(archivewire.Header) error, row func(Profile, []*string) error) (archivewire.Info, error) {
	table := -1
	// An archive from before usage orders (migration 61) names invoices,
	// skipped and refused with rows, and the tables usage orders renamed.
	invoices, preUsage := false, false
	// An archive from before rating at ingest (migration 66) carries the
	// period sweep's watermarks in place of the meter quantities: skipped.
	watermarks := false
	info, err := archivewire.Read(src, header, func(r archivewire.Record) error {
		if r.Kind == "table" {
			if invoices = r.Table == "invoices"; invoices {
				preUsage = true
				// One preceding invoice cadence retains no completed scan.
				if table+1 < len(Profiles) && Profiles[table+1].Name == "usage_collection_cadence" {
					table++
				}
				return nil
			}
			if watermarks = r.Table == "metered_rating_watermarks"; watermarks {
				r.Table = "meter_period_quantities"
			}
			if renamed, ok := usageRenames[r.Table]; ok {
				r.Table, preUsage = renamed, true
			}
			table++
			if table >= len(Profiles) || r.Table != Profiles[table].Name {
				return fmt.Errorf("invalid archive table order")
			}
			return nil
		}
		if invoices {
			return errArchivedInvoices
		}
		if watermarks {
			return nil
		}
		p := Profiles[table]
		if preUsage {
			if err := normalizePreUsage(p, &r.Values); err != nil {
				return err
			}
		}
		// Usage events before failed usage all succeeded and forgave nothing;
		// before rating at ingest none recorded charges.
		if p.Name == "usage_events" && len(r.Values) == len(p.Columns)-3 {
			r.Values = append(r.Values, new("succeeded"), new("0"))
		}
		if p.Name == "usage_events" && len(r.Values) == len(p.Columns)-1 {
			r.Values = append(r.Values, new("[]"))
		}
		if p.Name == "catalog_rate_cards" {
			ClearMeteredPaymentTerm(p, r.Values)
		}
		// Admissions before usage estimates held host-stated estimates only.
		if p.Name == "admission_operations" && len(r.Values) == len(p.Columns)-2 {
			r.Values = append(r.Values, nil, nil)
		}
		// Payment methods before card saves awaited the customer had no setup.
		if p.Name == "payment_methods" && len(r.Values) == len(p.Columns)-1 {
			r.Values = append(r.Values, nil)
		}
		// Orders before usage orders had none of their columns; before ui_mode
		// hosted checkouts only; before renewals none of their columns; before
		// hosted checkout none of its; before the payment axis one status.
		if p.Name == "orders" && len(r.Values) < len(p.Columns) {
			missing := len(p.Columns) - 2 - len(r.Values)
			if missing == orderUsageColumns+1+orderRenewalColumns+orderCheckoutColumns+1 {
				r.Values = splitOrderStatus(p, r.Values)
				missing--
			}
			switch missing {
			case orderUsageColumns:
				r.Values = append(r.Values, make([]*string, orderUsageColumns)...)
			case orderUsageColumns + 1, orderUsageColumns + 1 + orderRenewalColumns, orderUsageColumns + 1 + orderRenewalColumns + orderCheckoutColumns:
				r.Values = append(r.Values, make([]*string, missing-orderUsageColumns-1)...)
				r.Values = append(r.Values, hostedIfCheckout(p, r.Values))
				r.Values = append(r.Values, make([]*string, orderUsageColumns)...)
			}
			if len(r.Values) == len(p.Columns)-2 {
				r.Values = append(r.Values, new("engine"), nil)
			}
		}
		// Order lines before usage orders named no accrual source.
		if p.Name == "order_lines" {
			if len(r.Values) == len(p.Columns)-2 {
				r.Values = append(r.Values, nil)
			}
			if len(r.Values) == len(p.Columns)-1 {
				r.Values = append(r.Values, new("false"))
			}
		}
		// Rate cards before migration 66 carried a customer, set on a rate
		// override, which that migration deletes, and later a revision.
		if p.Name == "catalog_rate_cards" && len(r.Values) > len(p.Columns) {
			if r.Values[len(p.Columns)] != nil {
				return nil
			}
			r.Values = r.Values[:len(p.Columns)]
		}
		// Products and prices before migration 68 were never deleted.
		if (p.Name == "products" || p.Name == "prices") && len(r.Values) == len(p.Columns)-1 {
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

// The column groups later migrations appended to orders, in order: hosted
// checkout's, renewals', checkout_ui_mode, usage orders'.
const (
	orderCheckoutColumns = 5
	orderRenewalColumns  = 5
	orderUsageColumns    = 10
)

// hostedIfCheckout is the ui_mode of an order archived before it had one:
// hosted when it has a checkout.
func hostedIfCheckout(p Profile, values []*string) *string {
	for i, c := range p.Columns[:len(values)] {
		if c.Name == "checkout_secret_hash" && values[i] != nil {
			return new("hosted")
		}
	}
	return nil
}

// usageRenames are the tables usage orders renamed.
var usageRenames = map[string]string{"invoice_collection_cadence": "usage_collection_cadence", "invoice_items": "owed_items"}

var errArchivedInvoices = fmt.Errorf("an archive with invoices: restore it on a release before usage orders, then export it again")

// normalizePreUsage moves a row exported before usage orders to their
// shapes, as migration 61 moved the database. Rows naming an invoice are
// refused with the invoices.
func normalizePreUsage(p Profile, values *[]*string) error {
	row := *values
	switch p.Name {
	case "owed_items":
		if v := value(p, row, "order_id"); v != nil {
			return errArchivedInvoices
		}
	case "payments":
		// payments.invoice_id preceded ledger_transfer_id.
		if len(row) != len(p.Columns)+1 {
			return nil
		}
		at := slices.IndexFunc(p.Columns, func(c Column) bool { return c.Name == "ledger_transfer_id" })
		if row[at] != nil {
			return errArchivedInvoices
		}
		*values = slices.Delete(row, at, at+1)
	case "provider_intents":
		if typ := value(p, row, "intent_type"); typ != nil && *typ == "invoice_collection" {
			setValue(p, row, "intent_type", "order_collection")
		}
		if evidence := value(p, row, "result_evidence"); evidence != nil {
			setValue(p, row, "result_evidence", strings.Replace(*evidence, `"qualified_invoice_nonexecution":`, `"qualified_collection_nonexecution":`, 1))
		}
	case "host_outbox":
		if data := value(p, row, "data"); data != nil {
			setValue(p, row, "data", strings.Replace(*data, `"overdue_invoices":`, `"overdue_orders":`, 1))
		}
	}
	return nil
}

func setValue(p Profile, values []*string, name, v string) {
	for i, c := range p.Columns {
		if c.Name == name && i < len(values) {
			values[i] = &v
		}
	}
}

// splitOrderStatus is an order of one status as migration 49 split it: the
// order's own status and its payment's.
func splitOrderStatus(p Profile, values []*string) []*string {
	status, payment := "", "requires_payment_method"
	for i, c := range p.Columns[:len(values)] {
		if c.Name == "status" && values[i] != nil {
			status = *values[i]
			switch status {
			case "requires_action":
				values[i], payment = new("open"), "requires_action"
			case "processing":
				payment = "processing"
			case "paid":
				values[i], payment = new("complete"), "succeeded"
			}
		}
		if c.Name == "payment_id" && values[i] != nil && status != "requires_action" && status != "processing" {
			payment = "succeeded"
		}
	}
	return append(values, &payment)
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
