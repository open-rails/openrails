package billing_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// The receipt reads as openrails catalog apply has always printed it.
func TestCatalogApplicationReceiptString(t *testing.T) {
	pro, usd := "pro", "USD"
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	receipt := &billing.CatalogApplicationReceipt{
		ApplicationID: "cap_1", BaseRevision: 3, AppliedRevision: 5,
		Changes: []billing.CatalogChange{
			{Object: billing.CatalogObjectProduct, Key: "pro", Fields: []string{"display_name", "entitlements_spec"}, Revision: 4},
			{Object: billing.CatalogObjectPrice, Key: "monthly", ProductKey: &pro, Fields: []string{"unit_amount"}, Revision: 2},
		},
		Conflicts: []billing.CatalogConflict{
			{Object: billing.CatalogObjectPrice, Key: "yearly", ProductKey: &pro, Currency: (*catalog.Currency)(&usd), Fields: []billing.CatalogFieldConflict{
				{Field: "unit_amount", FileValue: json.RawMessage(`"99000000"`), LiveValue: json.RawMessage(`"89000000"`), SetBy: "console:alice", SetAt: at},
				{Field: "access_duration_hours", FileValue: json.RawMessage(`720`), LiveValue: json.RawMessage(`null`), SetBy: "console:alice", SetAt: at},
			}},
			{Object: billing.CatalogObjectMeter, Key: "tokens", Fields: []billing.CatalogFieldConflict{
				{Field: "display_name", FileValue: json.RawMessage(`"Tokens"`), LiveValue: json.RawMessage(`"AI tokens"`), SetBy: "api:bob", SetAt: at},
			}},
		},
		Warnings: []billing.CatalogWarning{
			{Key: "quarterly", ProductKey: pro, Refused: true, Message: "no Solana PSP configured"},
			{Key: "monthly", ProductKey: pro, Message: "Solana plan not published"},
		},
	}
	want := `cap_1: catalog revision 3 -> 5
  changed product pro (display_name, entitlements_spec), now revision 4
  changed price monthly of product pro (unit_amount), now revision 2
  price yearly of product pro skipped: unit_amount: the file says 99.00 USD, an edit set 89.00 USD (console:alice, 2026-11-01T12:00:00Z); access_duration_hours: the file says 30 days, an edit set none (console:alice, 2026-11-01T12:00:00Z)
  meter tokens skipped: display_name: the file says "Tokens", an edit set "AI tokens" (api:bob, 2026-11-01T12:00:00Z)
  refused price quarterly of product pro: no Solana PSP configured
  applied price monthly of product pro with warning: Solana plan not published
2 object(s) skipped: an edit set the fields above. Change the file to agree, remove those fields, or apply with force.
1 price(s) refused: document not recorded as applied. Fix the file and apply again.
`
	if got := receipt.String(); got != want {
		t.Fatalf("receipt =\n%s\nwant\n%s", got, want)
	}
	replayed := &billing.CatalogApplicationReceipt{ApplicationID: "cap_2", Replayed: true}
	if got, want := replayed.String(), "cap_2 was already applied; nothing changed\n"; got != want {
		t.Fatalf("replayed receipt = %q, want %q", got, want)
	}
	warned := &billing.CatalogApplicationReceipt{
		ApplicationID: "cap_3", BaseRevision: 5, AppliedRevision: 6,
		Warnings: []billing.CatalogWarning{
			{Key: "monthly", ProductKey: pro, Message: "Solana plan not published"},
		},
	}
	if got, want := warned.String(), "cap_3: catalog revision 5 -> 6\n  applied price monthly of product pro with warning: Solana plan not published\n"; got != want {
		t.Fatalf("warned receipt = %q, want %q", got, want)
	}
}
