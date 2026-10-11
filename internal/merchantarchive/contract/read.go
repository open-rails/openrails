package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/archivewire"
)

// ReadInfo keeps the original wire identity while identifying historical rows
// whose derived entitlement projection needs the same repair as migration 8,
// and archives whose per-key windows restore converts to product access.
type ReadInfo struct {
	archivewire.Info
	LegacyDurations bool
	LegacyAccess    bool
}

// Read applies the current billing schema and value contracts to the shared
// wire reader. Callers must roll back writes on any error, including a footer
// failure or missing table discovered after the final row.
func Read(src io.Reader, header func(archivewire.Header) error, row func(Profile, []*string) error) (ReadInfo, error) {
	table := -1
	legacyDurations := false
	profiles := Profiles
	version := 0
	onHeader := func(h archivewire.Header) error {
		version, profiles = h.Version, ProfilesFor(h.Version)
		if header != nil {
			return header(h)
		}
		return nil
	}
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
	// A version 1 archive's invoice_payments table is gone: its settled rows
	// are payments since migration 38, so one with rows is refused.
	invoicePayments := false
	// An archive from before usage orders (migration 61) names invoices,
	// skipped and refused with rows, and the tables usage orders renamed.
	invoices, preUsage := false, false
	// An archive from before rating at ingest (migration 66) carries the
	// period sweep's watermarks in place of the meter quantities: skipped.
	watermarks := false
	info, err := archivewire.Read(src, onHeader, func(r archivewire.Record) error {
		legacy := version == 1
		if r.Kind == "table" {
			if invoicePayments = legacy && r.Table == "invoice_payments"; invoicePayments {
				return nil
			}
			if invoices = r.Table == "invoices"; invoices {
				preUsage = true
				// One preceding invoice cadence retains no completed scan.
				if table+1 < len(profiles) && profiles[table+1].Name == "usage_collection_cadence" {
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
			if legacy && table >= 0 && profiles[table].Name == "products" {
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
			if table >= len(profiles) || r.Table != profiles[table].Name {
				return fmt.Errorf("invalid archive table order")
			}
			return nil
		}
		if invoicePayments {
			return fmt.Errorf("a version 1 archive with invoice payments: restore it on a release before orders, then export it again")
		}
		if invoices {
			return errArchivedInvoices
		}
		if watermarks {
			return nil
		}
		p := profiles[table]
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
		if p.Name == "orders" {
			missing := len(p.Columns) - len(r.Values)
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
		}
		// Order lines before usage orders named no accrual source.
		if p.Name == "order_lines" && len(r.Values) == len(p.Columns)-1 {
			r.Values = append(r.Values, nil)
		}
		// Rate cards before migration 66 carried a customer, set on a rate
		// override, which that migration deletes, and later a revision.
		if p.Name == "catalog_rate_cards" && len(r.Values) > len(p.Columns) {
			if r.Values[len(p.Columns)] != nil {
				return nil
			}
			r.Values = r.Values[:len(p.Columns)]
		}
		if !legacy {
			// Products and prices before migration 68 were never deleted.
			if (p.Name == "products" || p.Name == "prices") && len(r.Values) == len(p.Columns)-1 {
				r.Values = append(r.Values, nil)
			}
			return emit(p, r.Values)
		}
		// Version 1: rows of every earlier shape, normalized as the
		// migrations normalized the database.
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
		if p.Name == "payments" && len(r.Values) == len(p.Columns)-1 {
			r.Values = append(r.Values, nil)
		}
		// Prices before seats and before deletion: both trailing columns null.
		for p.Name == "prices" && len(r.Values) < len(p.Columns) && len(r.Values) >= len(p.Columns)-2 {
			r.Values = append(r.Values, nil)
		}
		if p.Name == "payments" && len(r.Values) == len(p.Columns)-2 {
			r.Values = append(r.Values, nil, nil) // Before purchased credits and private legacy duration evidence.
		}
		if p.Name == "payments" {
			renamePaymentStatus(p, r.Values)
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
		if p.Name == "subscriptions" || p.Name == "payments" || p.Name == "grants" {
			if err := ValidateValues(p, r.Values); err != nil {
				return err
			}
			current := namedProfile(p.Name)
			return emit(current, project(p, current, r.Values))
		}
		return emit(p, r.Values)
	})
	if err == nil && table != len(profiles)-1 {
		err = fmt.Errorf("archive tables incomplete")
	}
	return ReadInfo{Info: info, LegacyDurations: legacyDurations, LegacyAccess: version == 1}, err
}

// project reorders a row into another profile's columns by name; a column the
// source lacks is null.
func project(from, to Profile, values []*string) []*string {
	out := make([]*string, len(to.Columns))
	for i, c := range to.Columns {
		out[i] = value(from, values, c.Name)
	}
	return out
}

func insertRevision(values []*string) []*string {
	out := make([]*string, len(values)+1)
	out[0] = values[0]
	copy(out[2:], values[1:])
	return out
}

// flushProducts emits held products rows. Rows of an archive preceding
// product_entitlements carry their keys, which become product_entitlements
// rows valid from before key history, as migration 18 converts them.
func flushProducts(rows [][]*string, current bool, emit func(Profile, []*string) error) error {
	products := namedProfile("products")
	if current {
		for _, values := range rows {
			// Before ownership (derived) and before deletion.
			for len(values) < len(products.Columns) && len(values) >= len(products.Columns)-2 {
				values = append(values, nil)
			}
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
		current = append(current, nil, nil) // Before ownership (derived) and before deletion.
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

// renamePaymentStatus moves a legacy payment's completed to succeeded, as
// migration 38 moved the database.
func renamePaymentStatus(p Profile, values []*string) {
	for i, c := range p.Columns {
		if c.Name == "status" && i < len(values) && values[i] != nil && *values[i] == "completed" {
			values[i] = new("succeeded")
		}
	}
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
