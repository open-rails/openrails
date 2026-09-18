package contract

import (
	"strings"
	"testing"
)

func TestRebillArchiveRetainsQualifiedReceiptAndRefusesClaims(t *testing.T) {
	payload := `{"subscription_id":"10000000-0000-0000-0000-000000000001","period_end":"2026-09-18T00:00:00Z","rail":"nmi","order_reference":"rebill-original","attempt":1,"payment_method_id":"10000000-0000-0000-0000-000000000002","rail_subscription_id":"recurring-original","credential_reference":"sequence-original","credential_anchor_source":"agreement","instrument":{"psp_id":"10000000-0000-0000-0000-000000000003","custodian":"psp","rail_customer_ref":"vault-original","rail_method_ref":"billing-original"},"currency":"USD","amount":12000000,"amount_minor":1200,"request_key":"original-request"}`
	evidence := `{"transaction_id":"transaction-original","qualified_rebill_receipt":{"transaction_id":"transaction-original","binding":"` + strings.Repeat("a", 64) + `"}}`
	profile := Profile{Name: "rail_intents", Columns: []Column{{"intent_type", "text"}, {"status", "text"}, {"payload", "jsonb"}, {"result_evidence", "jsonb"}, {"claimed_until", "timestamp with time zone"}}}
	typ, status := "manual_rebill", "succeeded"
	row := []*string{&typ, &status, &payload, &evidence, nil}
	if err := ValidateValues(profile, row); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{}`, `null`,
		`{"qualified_rebill_receipt":{"transaction_id":"transaction-original","binding":"bad"}}`,
		`{"qualified_rebill_receipt":{"transaction_id":"transaction-original","binding":"` + strings.Repeat("A", 64) + `"}}`,
		`{"qualified_rebill_receipt":{"transaction_id":"transaction-original","binding":"` + strings.Repeat("a", 64) + `","raw_body":"secret"}}`,
		`{"transaction_id":"candidate-only"}`,
	} {
		row[3] = &bad
		if ValidateValues(profile, row) == nil {
			t.Fatalf("accepted malformed or unqualified evidence %s", bad)
		}
	}
	row[3] = &evidence
	for _, bad := range []string{`{}`, `null`, `{"unvaulted":true}`, `{"instrument":{"security_key":"secret"}}`, `{"raw_response":"provider bytes"}`} {
		row[2] = &bad
		if ValidateValues(profile, row) == nil {
			t.Fatalf("accepted unsupported rebill payload %s", bad)
		}
	}
	row[2] = nil
	if ValidateValues(profile, row) == nil {
		t.Fatal("accepted missing retained payload")
	}
	for _, column := range []Column{{"dunning_claim_holder", "text"}, {"dunning_claimed_until", "timestamp with time zone"}} {
		p := Profile{Name: "subscriptions", Columns: []Column{column}}
		value := "expired-owner"
		if column.Name == "dunning_claimed_until" {
			value = "2000-01-01 00:00:00+00"
		}
		if ValidateValues(p, []*string{&value}) == nil {
			t.Fatalf("accepted execution claim %s", column.Name)
		}
		if err := ValidateValues(p, []*string{nil}); err != nil {
			t.Fatal(err)
		}
	}
}
