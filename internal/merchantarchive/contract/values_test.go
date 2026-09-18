package contract

import (
	"testing"
)

const testMerchant = "10000000-0000-0000-0000-000000000001"

func TestScalarPrecisionAndSecretContracts(t *testing.T) {
	p := Profile{Name: "ledger_accounts", Columns: []Column{{"merchant_id", "uuid"}, {"credits_posted", "bigint"}}}
	v := "9007199254740993"
	mid := testMerchant
	if err := ValidateValues(p, []*string{&mid, &v}); err != nil {
		t.Fatal(err)
	}
	v = "9223372036854775808"
	if ValidateValues(p, []*string{&mid, &v}) == nil {
		t.Fatal("accepted overflow")
	}
	for _, raw := range []string{`{"api_key":"test"}`, `{"profile":{"secret":"test"}}`, `{"unknown":42}`} {
		if validateJSON("merchant_configurations.config", raw) == nil {
			t.Fatal("accepted unknown/secret field")
		}
	}
	if err := validateJSON("usage_events.dimensions", `{"tokens":9007199254740993}`); err != nil {
		t.Fatal(err)
	}
}

func TestNestedContractsRejectRawFieldsAndPreserveIndefiniteAccess(t *testing.T) {
	for _, raw := range []string{
		`{"profile":{"cardNumber":"4111111111111111","apiKey":"sk_live_review_canary"}}`,
		`{"profile":{"secret":"review_canary"},"profile":{"display_name":"normal"}}`,
		`{"profile collection_threshold":42}`,
		`{"collection_threshold":"not-money"}`,
		`{"profile":{"display_name":"4111111111111111"}}`,
	} {
		if validateJSON("merchant_configurations.config", raw) == nil {
			t.Fatal("accepted unsafe contract", raw)
		}
	}
	if validateJSON("usage_events.dimensions", `{"tokens":1e100}`) == nil {
		t.Fatal("accepted unbounded measurement")
	}
	for _, field := range []string{"products.entitlements_spec", "subscriptions.entitlements_spec_snapshot", "payments.entitlements_spec_snapshot"} {
		if err := validateJSON(field, `{"premium":null,"secret_content":24}`); err != nil {
			t.Fatal(field, err)
		}
	}
	if err := validateJSON("custodians.settings", `{"public_api_key":"public","account_updater":true,"account_updater_lookahead_days":"30"}`); err != nil {
		t.Fatal(err)
	}
	if validateJSON("admission_operations.capture_terms", `{"metadata":{"opaque":"replay-fact"}}`) == nil {
		t.Fatal("accepted unsupported opaque replay metadata")
	}
	for _, v := range []string{"now", "infinity", "2026-01-01", "-----BEGIN PRIVATE KEY-----"} {
		p := Profile{Name: "customers", Columns: []Column{{"created_at", "timestamp with time zone"}}}
		if ValidateValues(p, []*string{&v}) == nil {
			t.Fatal("accepted nonabsolute timestamp")
		}
	}
}

func TestCanonicalTimestampAndArraySafety(t *testing.T) {
	for _, v := range []string{"2026-01-01 00:00:00+00", "2026-01-01 00:00:00.123456+00"} {
		if err := ValidateValues(Profile{Name: "customers", Columns: []Column{{"created_at", "timestamptz"}}}, []*string{&v}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"2026-01-01 00:00:00.1234567+00", "2026-01-01 00:00:00.100000+00"} {
		if ValidateValues(Profile{Name: "customers", Columns: []Column{{"created_at", "timestamptz"}}}, []*string{&v}) == nil {
			t.Fatal("accepted noncanonical timestamp")
		}
	}
	for _, v := range []string{`{}`, `{USD,daily}`, `{"USD,daily","quoted\"key","back\\slash"}`} {
		if !safeTextArray(v) {
			t.Fatal("refused safe array", v)
		}
	}
	for _, v := range []string{`[2:2]={x}`, `{{x}}`, `{NULL}`, `{x,}`, `{x,4111111111111111}`, `{sk_live_test}`, `{"unclosed}`} {
		if safeTextArray(v) {
			t.Fatal("accepted invalid or sensitive array", v)
		}
	}
	for _, field := range []string{"catalog_meters.group_by", "catalog_rate_cards.filter"} {
		if err := validateJSON(field, "null"); err != nil {
			t.Fatal(field, err)
		}
	}
	if !safeText("12345678-1234-1234-1234-123456789012") {
		t.Fatal("UUID mistaken for PAN")
	}
}

func TestSubscriptionGatewayMetadataContract(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`,
		`{"order_id":"checkout-1","provider_transaction_id":"charge-1","delayed_start":"2027-01-01T00:00:00Z","e2e_run_id":"run-1","admin_notes":"billing note"}`,
		`{"order_id":"checkout-1","superseded_at":"2026-09-17T00:00:00Z","superseded_by_subscription_id":"10000000-0000-0000-0000-000000000001"}`,
		`{"previous_gateway_response":null,"superseded_at":"2026-09-17T00:00:00Z","superseded_by_subscription_id":null}`,
	} {
		if err := validateJSON("subscriptions.gateway_response", raw); err != nil {
			t.Errorf("refused supported subscription metadata %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"order_id":"checkout-1","unknown":"value"}`, `{"raw_body":{"order_id":"checkout-1"}}`,
		`{"order_id":42}`, `{"provider_transaction_id":"sk_live_secret"}`,
		`{"previous_gateway_response":{"secret":"unsafe"}}`, `[]`,
	} {
		if validateJSON("subscriptions.gateway_response", raw) == nil {
			t.Errorf("accepted unsafe subscription metadata %s", raw)
		}
	}
}
