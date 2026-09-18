package contract

import "testing"

func TestInvoiceCollectionArchiveContract(t *testing.T) {
	payload := `{"invoice_id":"10000000-0000-0000-0000-000000000001","customer_id":"10000000-0000-0000-0000-000000000002","attempt_id":"10000000-0000-0000-0000-000000000003","payment_method_id":"10000000-0000-0000-0000-000000000004","rail":"nmi","currency":"USD","amount":50000,"amount_minor":5,"description":"invoice","instrument":{"psp_id":"10000000-0000-0000-0000-000000000005","custodian":"psp","rail_customer_ref":"vault-original","rail_method_ref":"billing-original"}}`
	if err := validateJSON("rail_intents.invoice_collection.payload", payload); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"instrument":{"psp_id":"not-a-uuid"}}`,
		`{"instrument":{"security_key":"secret"}}`,
		`{"instrument":{"pan":"4111111111111111"}}`,
		`{"provider_body":{}}`,
	} {
		if err := validateJSON("rail_intents.invoice_collection.payload", bad); err == nil {
			t.Fatalf("accepted unsupported collection payload %s", bad)
		}
	}
	profile := Profile{Name: "invoices", Columns: []Column{{Name: "collection_intent_id", Type: "uuid"}}}
	if err := ValidateValues(profile, []*string{nil}); err != nil {
		t.Fatal(err)
	}
	id := "10000000-0000-0000-0000-000000000001"
	if err := ValidateValues(profile, []*string{&id}); err == nil {
		t.Fatal("accepted invoice with an unresolved collection pointer")
	}
}
