package contract

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/google/uuid"

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
	// period sweep's watermarks in place of the meter quantities, one from
	// before migration 207 the custody migration record, and one from before
	// migration 208 the usage collection cadence: skipped.
	watermarks, custody, cadence := false, false, false
	// An archive from before holds carries admissions, each a request hold.
	admissions := false
	// An archive from before usage settled in batches carries raw usage
	// events: their money is in the ledger it carries, and they are not read.
	events := false
	info, err := archivewire.Read(src, header, func(r archivewire.Record) error {
		if r.Kind == "table" {
			invoices = r.Table == "invoices"
			cadence = r.Table == "usage_collection_cadence" || r.Table == "invoice_collection_cadence"
			if invoices || cadence {
				preUsage = preUsage || r.Table != "usage_collection_cadence"
				return nil
			}
			if custody = r.Table == "custody_migrations"; custody {
				return nil
			}
			if events = r.Table == "usage_events"; events {
				return nil
			}
			if watermarks = r.Table == "metered_rating_watermarks"; watermarks {
				r.Table = "meter_period_quantities"
			}
			if admissions = r.Table == "admission_operations"; admissions {
				r.Table = "limit_holds"
			}
			if renamed, ok := usageRenames[r.Table]; ok {
				r.Table, preUsage = renamed, true
			}
			if table+1 < len(Profiles) && Profiles[table+1].Name == "subscription_billing_changes" && r.Table == "subscription_status_transitions" {
				table++
			}
			for table+1 < len(Profiles) && addedTables[Profiles[table+1].Name] && r.Table != Profiles[table+1].Name {
				table++
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
		if watermarks || custody || cadence || events {
			return nil
		}
		p := Profiles[table]
		if admissions {
			values, err := holdFromAdmission(r.Values)
			if err != nil {
				return err
			}
			r.Values = values
		}
		if err := dropUnreadColumns(p, &r.Values); err != nil {
			return err
		}
		// Older exports always carried an explicit credit limit, including
		// zero. Preserve that meaning; no schedule existed before #1289.
		if p.Name == "money_settings" && len(r.Values) == len(p.Columns)-5 {
			r.Values = append(r.Values, new("true"), nil, nil, nil, new("false"))
		}
		if p.Name == "subscription_status_transitions" && len(r.Values) == len(p.Columns)-4 {
			r.Values = append(r.Values, nil, nil, nil, nil)
		}
		if preUsage {
			if err := normalizePreUsage(p, &r.Values); err != nil {
				return err
			}
		}
		// Holds before batch settlement kept no captured charges, and before
		// versioned answers no versions: a request's hold has neither.
		if p.Name == "limit_holds" && len(r.Values) < len(p.Columns) && len(r.Values) >= len(p.Columns)-3 {
			r.Values = append(r.Values, make([]*string, len(p.Columns)-len(r.Values))...)
		}
		if p.Name == "catalog_rate_cards" {
			ClearMeteredPaymentTerm(p, r.Values)
		}
		// Orders before checkout facts had none of their columns; before
		// usage orders none of theirs; before embedded pay URLs hosted ones
		// only; before renewals none of their columns; before pay URLs none
		// of theirs; before the payment axis one status. Before migration 89,
		// embedded was a ui_mode.
		if p.Name == "orders" && len(r.Values) < len(p.Columns) {
			base := Profile{Name: p.Name, Columns: p.Columns[:len(p.Columns)-orderCheckoutColumns]}
			missing := len(base.Columns) - 2 - len(r.Values)
			if missing == orderUsageColumns+1+orderRenewalColumns+orderPayURLColumns+1 {
				r.Values = splitOrderStatus(base, r.Values)
				missing--
			}
			switch missing {
			case orderUsageColumns:
				r.Values = append(r.Values, make([]*string, orderUsageColumns)...)
			case orderUsageColumns + 1, orderUsageColumns + 1 + orderRenewalColumns, orderUsageColumns + 1 + orderRenewalColumns + orderPayURLColumns:
				r.Values = append(r.Values, make([]*string, missing-orderUsageColumns-1)...)
				r.Values = append(r.Values, hostedIfPayURL(base, r.Values))
				r.Values = append(r.Values, make([]*string, orderUsageColumns)...)
			}
			if len(r.Values) == len(base.Columns)-2 {
				r.Values = append(r.Values, new("engine"), nil)
			}
			if len(r.Values) == len(base.Columns) {
				r.Values = append(r.Values, make([]*string, orderCheckoutColumns)...)
			}
		}
		if p.Name == "orders" {
			embeddedFromUIMode(p, r.Values)
		}
		// Subscriptions before checkout facts named no checkout.
		if p.Name == "subscriptions" && len(r.Values) == len(p.Columns)-1 {
			r.Values = append(r.Values, nil)
		}
		// Order lines before usage orders named no accrual source.
		if p.Name == "order_lines" {
			if len(r.Values) == len(p.Columns)-3 {
				r.Values = append(r.Values, nil)
			}
			if len(r.Values) == len(p.Columns)-2 {
				r.Values = append(r.Values, new("false"))
			}
			if len(r.Values) == len(p.Columns)-1 {
				r.Values = append(r.Values, nil)
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
		// Products and prices before migration 68 were never deleted; products
		// before migration 88 required no entitlements.
		if p.Name == "products" && len(r.Values) == len(p.Columns)-2 {
			r.Values = append(r.Values, nil)
		}
		if p.Name == "products" && len(r.Values) == len(p.Columns)-1 {
			none := "{}"
			r.Values = append(r.Values, &none)
		}
		if p.Name == "prices" && len(r.Values) != len(p.Columns) {
			return fmt.Errorf("archive prices lack explicit ownership: restore using OpenRails v0.255.0, then upgrade")
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

// The column groups later migrations appended to orders, in order: the pay
// URL's, renewals', embedded, usage orders', checkout facts.
const (
	orderPayURLColumns   = 5
	orderRenewalColumns  = 5
	orderUsageColumns    = 10
	orderCheckoutColumns = 13
)

// hostedIfPayURL is embedded for an order archived before it had one: false
// (hosted) when it has a pay URL.
func hostedIfPayURL(p Profile, values []*string) *string {
	for i, c := range p.Columns[:len(values)] {
		if c.Name == "pay_url_secret_hash" && values[i] != nil {
			return new("false")
		}
	}
	return nil
}

// embeddedFromUIMode reads an order archived before migration 89, whose
// embedded column was its ui_mode, hosted or embedded.
func embeddedFromUIMode(p Profile, values []*string) {
	for i, c := range p.Columns[:min(len(p.Columns), len(values))] {
		if c.Name != "embedded" || values[i] == nil {
			continue
		}
		switch *values[i] {
		case "hosted":
			values[i] = new("false")
		case "embedded":
			values[i] = new("true")
		}
	}
}

// holdFromAdmission is an archived admission as its request hold. Its id is
// derived from the admission's identity, so a restore reads the same rows.
// Admissions before usage estimates have no estimate_usage or rate_cards.
func holdFromAdmission(v []*string) ([]*string, error) {
	if len(v) != 16 && len(v) != 18 {
		return nil, fmt.Errorf("invalid archived admission")
	}
	at := func(i int) *string {
		if i < len(v) {
			return v[i]
		}
		return nil
	}
	if v[0] == nil || v[1] == nil || v[9] == nil {
		return nil, fmt.Errorf("invalid archived admission")
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte("openrails.admission:"+*v[0]+":"+*v[1]+":"+*v[9])).String()
	updated := v[9]
	for _, t := range []*string{v[14], v[15]} {
		if t != nil {
			updated = t
		}
	}
	return []*string{v[0], &id, v[9], v[2], v[3], nil, nil, v[1], v[4], v[5], v[6], at(16), at(17), v[7], v[8], v[11], v[12], v[13], v[14], v[15], updated, nil}, nil
}

// addedTables are tables an archive from before them lacks; it holds none
// of their rows.
var addedTables = map[string]bool{"change_log": true, "catalog_limits": true, "catalog_product_limits": true, "customer_limit_overrides": true, "limit_windows": true, "limit_adjustments": true,
	"usage_daily": true, "usage_accruals": true}

// usageRenames are the tables usage orders renamed.
var usageRenames = map[string]string{"invoice_items": "owed_items"}

// dropUnreadColumns removes from a row exported before migration 208 the
// values of the columns it dropped, each of which an archive held empty.
func dropUnreadColumns(p Profile, values *[]*string) error {
	var empty map[int]*string
	switch {
	case p.Name == "maintenance_runs" && len(*values) == len(p.Columns)+5:
		// mode, rails, window_starts_at, window_ends_at; dry_run.
		empty = map[int]*string{5: nil, 6: new("{}"), 7: nil, 8: nil, 12: new("false")}
	case p.Name == "order_attempts" && len(*values) == len(p.Columns)+1:
		empty = map[int]*string{len(p.Columns) - 1: nil} // routing_reason
	case p.Name == "payment_methods" && len(*values) == len(p.Columns)+1:
		empty = map[int]*string{len(p.Columns): nil} // wallet_card_id
	default:
		return nil
	}
	kept := make([]*string, 0, len(p.Columns))
	for i, v := range *values {
		want, dropped := empty[i]
		if !dropped {
			kept = append(kept, v)
		} else if (v == nil) != (want == nil) || v != nil && *v != *want {
			return fmt.Errorf("archive %s carries a value in a dropped column", p.Name)
		}
	}
	*values = kept
	return nil
}

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
