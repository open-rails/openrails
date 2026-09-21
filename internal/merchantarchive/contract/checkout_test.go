package contract

import "testing"

func TestCheckoutIntentReceiptContracts(t *testing.T) {
	p := Profile{Name: "rail_intents", Columns: []Column{{"intent_type", "text"}, {"status", "text"}, {"payload", "jsonb"}, {"result_evidence", "jsonb"}}}
	for _, tc := range []struct {
		name, typ, status, payload, evidence string
		valid                                bool
	}{
		{"pruned sale has no accepted terms", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","transaction_id":"sale-1"}`, false},
		{"pruned subscription has no accepted terms", "nmi_subscription_create", "succeeded", "{}", `{"subscription_id":"` + testMerchant + `","transaction_id":"sale-1","status":"success","message":"Subscription created successfully"}`, false},
		{"old boolean is not sale custody", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","transaction_id":"sale-1","verified_existing":true}`, false},
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
