package contract

import "testing"

func TestCheckoutIntentReceiptContracts(t *testing.T) {
	p := Profile{Name: "rail_intents", Columns: []Column{{"intent_type", "text"}, {"status", "text"}, {"payload", "jsonb"}, {"result_evidence", "jsonb"}}}
	for _, tc := range []struct {
		name, typ, status, payload, evidence string
		valid                                bool
	}{
		{"sale", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","transaction_id":"sale-1"}`, true},
		{"subscription", "nmi_subscription_create", "succeeded", "{}", `{"subscription_id":"` + testMerchant + `","transaction_id":"sale-1","status":"success","message":"Subscription created successfully"}`, true},
		{"verified sale", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","transaction_id":"sale-1","verified_existing":true}`, true},
		{"unknown result", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","raw_body":{}}`, false},
		{"credential result", "nmi_subscription_create", "succeeded", "null", `{"security_key":"secret"}`, false},
		{"credential in known field", "nmi_sale", "succeeded", "null", `{"transaction_id":"sk_live_secret"}`, false},
		{"unqualified sale payload", "nmi_sale", "failed_terminal", `{"customer_vault_id":"vault-1"}`, `{}`, false},
		{"ephemeral subscription token", "nmi_subscription_create", "failed_terminal", `{"payment_token":"token"}`, `{}`, false},
		{"succeeded sale still cannot carry payload", "nmi_sale", "succeeded", `{"original_payment_id":"` + testMerchant + `"}`, `{}`, false},
		{"succeeded subscription still cannot carry payload", "nmi_subscription_create", "succeeded", `{"amount_micros":2500000}`, `{}`, false},
		{"wrong id type", "nmi_sale", "succeeded", "null", `{"payment_id":42}`, false},
		{"wrong marker type", "nmi_sale", "succeeded", "null", `{"verified_existing":"true"}`, false},
		{"wrong code type", "nmi_subscription_create", "failed_terminal", "null", `{"response_code":"100"}`, false},
		{"malformed id", "nmi_subscription_create", "succeeded", "null", `{"subscription_id":"not-a-uuid"}`, false},
		{"unrelated intent", "nmi_refund", "succeeded", "null", `{"payment_id":"` + testMerchant + `"}`, false},
		{"unresolved", "nmi_sale", "unknown_needs_verify", "null", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateValues(p, []*string{&tc.typ, &tc.status, &tc.payload, &tc.evidence})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestPaymentCorrelationMetadataContract(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"order_id":"checkout-order","provider_transaction_id":"sale-1"}`,
		`{"stripe_invoice_id":"in_paid","e2e_run_id":"run-1"}`,
	} {
		if err := validateJSON("payments.metadata", raw); err != nil {
			t.Fatalf("refused known correlation %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"unknown":"value"}`, `{"security_key":"secret"}`, `{"order_id":{"raw":"body"}}`,
		`{"provider_transaction_id":"sk_test_secret"}`, `{"order_id":"4111111111111111"}`,
	} {
		if validateJSON("payments.metadata", raw) == nil {
			t.Fatalf("accepted unqualified metadata %s", raw)
		}
	}
}

// An invoice collection archives its frozen charge and scalar receipt facts;
// a provider body, a credential or an unknown key is refused.
func TestInvoiceCollectionOperationContracts(t *testing.T) {
	p := Profile{Name: "rail_intents", Columns: []Column{{"intent_type", "text"}, {"status", "text"}, {"payload", "jsonb"}, {"result_evidence", "jsonb"}}}
	frozen := `{"invoice_id":"` + testMerchant + `","customer_id":"` + testMerchant + `","attempt_id":"` + testMerchant +
		`","payment_method_id":"` + testMerchant + `","rail":"nmi","currency":"USD","amount":4000000,"amount_minor":400,` +
		`"description":"Invoice INV-1","instrument":{"psp_id":"` + testMerchant + `","custodian":"nmi","rail_customer_ref":"vault-1","rail_method_ref":"card-1"}}`
	for _, tc := range []struct {
		name, typ, status, payload, evidence string
		valid                                bool
	}{
		{"settled", "invoice_collection", "succeeded", frozen, `{"transaction_id":"sale-1","rail":"nmi","external_invoice_id":"in_1"}`, true},
		{"declined", "invoice_collection", "failed_terminal", frozen, `{"declined":true,"failure_code":"200","rail":"nmi"}`, true},
		{"not executed", "invoice_collection", "failed_terminal", frozen, `{"not_executed":true,"declined":false,"not_executed_code":"instrument_changed","failure_message":"payment method changed"}`, true},
		{"operator resolved", "invoice_collection", "succeeded", frozen, `{"transaction_id":"sale-1","rail":"nmi","operator_resolution":{"actor":"ops","reason":"receipt","resolved_at":"2026-09-18T00:00:00Z","provider_reference":"sale-1"}}`, true},
		{"custodian held", "invoice_collection", "succeeded", `{"invoice_id":"` + testMerchant + `","instrument":{"psp_id":"` + testMerchant + `","custodian":"openrails","custodian_id":"` + testMerchant + `","rail_customer_ref":"","rail_method_ref":"tok-1"}}`, `{}`, true},
		{"provider body", "invoice_collection", "succeeded", frozen, `{"transaction_id":"sale-1","raw_response":{"amount":"40.00"}}`, false},
		{"credential in receipt", "invoice_collection", "succeeded", frozen, `{"transaction_id":"sk_live_secret"}`, false},
		{"unknown payload key", "invoice_collection", "succeeded", `{"customer_vault_id":"vault-1"}`, `{}`, false},
		{"unknown instrument key", "invoice_collection", "succeeded", `{"instrument":{"security_key":"secret"}}`, `{}`, false},
		{"numeric money as text", "invoice_collection", "succeeded", `{"amount":"4000000"}`, `{}`, false},
		{"malformed instrument id", "invoice_collection", "succeeded", `{"instrument":{"psp_id":"not-a-uuid"}}`, `{}`, false},
		{"unresolved", "invoice_collection", "unknown_needs_verify", frozen, `{"submitted_at":"2026-09-18T00:00:00Z"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateValues(p, []*string{&tc.typ, &tc.status, &tc.payload, &tc.evidence})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
