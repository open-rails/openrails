package contract

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
)

const testMerchant = "10000000-0000-0000-0000-000000000001"

func profile(t *testing.T, name string) Profile {
	t.Helper()
	for _, p := range Profiles {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no profile %s", name)
	return Profile{}
}

// row fills a full production profile; unnamed columns are SQL NULL.
func row(t *testing.T, name string, fields map[string]string) (Profile, []*string) {
	t.Helper()
	p := profile(t, name)
	values := make([]*string, len(p.Columns))
	for i, c := range p.Columns {
		if v, ok := fields[c.Name]; ok {
			values[i] = &v
		}
	}
	for f := range fields {
		if value(p, values, f) == nil {
			t.Fatalf("%s has no column %s", name, f)
		}
	}
	return p, values
}

type rowCase struct {
	name, table string
	fields      map[string]string
	valid       bool
}

func checkRows(t *testing.T, cases []rowCase) {
	t.Helper()
	for _, tc := range cases {
		p, values := row(t, tc.table, tc.fields)
		if err := ValidateValues(p, values); (err == nil) != tc.valid {
			t.Errorf("%s: valid=%v, error=%v", tc.name, tc.valid, err)
		}
	}
}

func TestScalarContracts(t *testing.T) {
	ts := func(v string) map[string]string { return map[string]string{"created_at": v} }
	checkRows(t, []rowCase{
		{"int64 beyond 2^53", "ledger_accounts", map[string]string{"merchant_id": testMerchant, "credits_posted": "9007199254740993"}, true},
		{"int64 overflow", "ledger_accounts", map[string]string{"credits_posted": "9223372036854775808"}, false},
		{"leading zero", "ledger_accounts", map[string]string{"credits_posted": "01"}, false},
		{"explicit plus", "ledger_accounts", map[string]string{"credits_posted": "+1"}, false},
		{"int32 max", "prices", map[string]string{"access_duration_hours": "2147483647"}, true},
		{"int32 overflow", "prices", map[string]string{"access_duration_hours": "2147483648"}, false},
		{"boolean spelling", "prices", map[string]string{"archived": "t"}, false},
		{"uppercase uuid", "prices", map[string]string{"id": strings.ToUpper("abcdef00-0000-0000-0000-000000000001")}, false},
		{"credit pseudo-currency", "prices", map[string]string{"currency": "credit:USD"}, false},
		{"utc timestamp", "customers", ts("2026-01-01 00:00:00+00"), true},
		{"microsecond timestamp", "customers", ts("2026-01-01 00:00:00.123456+00"), true},
		{"nanosecond timestamp", "customers", ts("2026-01-01 00:00:00.1234567+00"), false},
		{"trailing zero fraction", "customers", ts("2026-01-01 00:00:00.100000+00"), false},
		{"non-utc offset", "customers", ts("2026-01-01 00:00:00+01"), false},
		{"date only", "customers", ts("2026-01-01"), false},
		{"now", "customers", ts("now"), false},
		{"infinity", "customers", ts("infinity"), false},
		{"PEM in timestamp", "customers", ts("-----BEGIN PRIVATE KEY-----"), false},
		{"PAN in text", "customers", map[string]string{"issuer": "4111111111111111"}, false},
		{"secret key in text", "customers", map[string]string{"issuer": "sk_live_x"}, false},
		{"uuid is not a PAN", "customers", map[string]string{"issuer": "12345678-1234-1234-1234-123456789012"}, true},
		{"text array", "maintenance_runs", map[string]string{"rails": `{nmi,stripe}`, "kind": "prune"}, true},
		{"nested text array", "maintenance_runs", map[string]string{"rails": `{{nmi}}`}, false},
		{"opaque owner subject", "catalogs", map[string]string{"owner_subject": "https://issuer.invalid/作者?identity=Case%2f#value"}, true},
		{"PAN-shaped owner subject is opaque identity", "catalogs", map[string]string{"owner_subject": "4242424242424242"}, true},
		{"empty owner subject", "catalogs", map[string]string{"owner_subject": ""}, false},
		{"NUL owner subject", "catalogs", map[string]string{"owner_subject": "a\x00b"}, false},
		{"default catalog owner", "catalogs", map[string]string{"merchant_id": testMerchant}, true},
	})
	if err := ValidateValues(profile(t, "customers"), []*string{nil}); err == nil {
		t.Fatal("row width mismatch accepted")
	}
}

// Only settled, terminal state is portable; anything in flight at cutover
// would be resumed by nobody on the destination.
func TestOnlyTerminalStateIsPortable(t *testing.T) {
	done := "2026-01-01 00:00:00+00"
	checkRows(t, []rowCase{
		{"pending payment", "payments", map[string]string{"status": "pending"}, false},
		{"settled payment", "payments", map[string]string{"status": "succeeded"}, true},
		{"attempted invoice payment", "invoice_payments", map[string]string{"status": "attempted"}, false},
		{"open checkout", "checkout_sessions", map[string]string{"status": "open"}, false},
		{"expired checkout", "checkout_sessions", map[string]string{"status": "expired"}, true},
		{"admitted admission", "admission_operations", map[string]string{"state": "admitted"}, false},
		{"captured admission", "admission_operations", map[string]string{"state": "captured"}, true},
		{"claimed intent", "rail_intents", map[string]string{"status": "claimed"}, false},
		{"unresolved intent", "rail_intents", map[string]string{"status": "unknown_needs_verify"}, false},
		{"superseded intent", "rail_intents", map[string]string{"status": "superseded"}, true},
		{"running maintenance", "maintenance_runs", map[string]string{"kind": "prune", "status": "running"}, false},
		{"unportable maintenance kind", "maintenance_runs", map[string]string{"kind": "reconcile"}, false},
		{"unfinished webhook", "webhook_events", map[string]string{"op": "x"}, false},
		{"finished webhook", "webhook_events", map[string]string{"op": "x", "completed_at": done}, true},
		{"undelivered host event", "host_outbox", map[string]string{"event_type": "delinquency.entered"}, false},
		{"live invoice collection", "invoices", map[string]string{"collection_intent_id": testMerchant}, false},
		{"resolved invoice", "invoices", map[string]string{"status": "paid"}, true},
	})
}

func TestSubscriptionCollectionOwnership(t *testing.T) {
	sub := func(policy, rail, binding string) map[string]string {
		m := map[string]string{"rail": rail, "rail_subscription_id": binding}
		if policy != "" {
			m["collection_policy"] = policy
		}
		return m
	}
	checkRows(t, []rowCase{
		{"provider schedule", "subscriptions", sub("provider", "stripe", "sub_1"), true},
		{"nmi schedule", "subscriptions", sub("nmi_schedule", "nmi", "sub-1"), true},
		{"stripe nmi schedule", "subscriptions", sub("nmi_schedule", "stripe", "sub_1"), false},
		{"nmi provider", "subscriptions", sub("provider", "nmi", "sub-1"), false},
		{"engine nmi unbound", "subscriptions", sub("engine", "nmi", ""), true},
		{"engine nmi with provider schedule", "subscriptions", sub("engine", "nmi", "sub-1"), false},
		{"engine solana", "subscriptions", sub("engine", "solana", "pda"), true},
		{"engine ccbill", "subscriptions", sub("engine", "ccbill", ""), false},
		{"missing policy", "subscriptions", sub("", "nmi", ""), false},
		{"unknown policy", "subscriptions", sub("manual", "nmi", ""), false},
	})
}

func TestCatalogApplicationReceiptMatchesRow(t *testing.T) {
	catalog := uuid.MustParse("20000000-0000-0000-0000-000000000001")
	base := func() map[string]string {
		return map[string]string{
			"application_id": "deploy-1", "catalog_id": catalog.String(), "request_sha256": `\x` + strings.Repeat("ab", 32),
			"base_revision": "4", "applied_revision": "5", "applied_at": "2026-01-01 00:00:00+00",
			"result": `{"application_id":"deploy-1","catalog_id":"` + openrails.CatalogID(catalog).String() + `","base_revision":4,"applied_revision":5,"replayed":false,"products_changed":1,"prices_changed":0}`,
		}
	}
	cases := []rowCase{{"exact receipt", "catalog_applications", base(), true}}
	for name, mutate := range map[string]func(map[string]string){
		"other application": func(m map[string]string) { m["application_id"] = "deploy-2" },
		"skipped revision":  func(m map[string]string) { m["applied_revision"] = "6" },
		"other catalog":     func(m map[string]string) { m["catalog_id"] = testMerchant },
		"short digest":      func(m map[string]string) { m["request_sha256"] = `\xab` },
		"non-hex digest":    func(m map[string]string) { m["request_sha256"] = `\x` + strings.Repeat("zz", 32) },
		"replayed receipt": func(m map[string]string) {
			m["result"] = strings.Replace(m["result"], `"replayed":false`, `"replayed":true`, 1)
		},
		"negative change": func(m map[string]string) {
			m["result"] = strings.Replace(m["result"], `"prices_changed":0`, `"prices_changed":-1`, 1)
		},
		"missing result": func(m map[string]string) { delete(m, "result") },
		"unprefixed catalog": func(m map[string]string) {
			m["result"] = strings.Replace(m["result"], openrails.CatalogID(catalog).String(), catalog.String(), 1)
		},
	} {
		m := base()
		mutate(m)
		cases = append(cases, rowCase{name, "catalog_applications", m, false})
	}
	checkRows(t, cases)
}

func TestJSONContracts(t *testing.T) {
	for _, tc := range []struct {
		field string
		ok    []string
		bad   []string
	}{
		{"merchant_configurations.config", nil, []string{
			`{"api_key":"test"}`, `{"profile":{"secret":"test"}}`, `{"unknown":42}`,
			`{"profile":{"secret":"x"},"profile":{"display_name":"normal"}}`,
			`{"profile collection_threshold":42}`, `{"collection_threshold":"not-money"}`,
			`{"profile":{"display_name":"4111111111111111"}}`,
		}},
		{"usage_events.dimensions", []string{`{"tokens":9007199254740993}`}, []string{`{"tokens":1e100}`, `{"tokens":1.5}`, `{"tokens":1} {}`}},
		{"products.entitlements_spec", []string{`null`, `{"premium":null,"secret_content":24}`}, []string{`{"premium":"1"}`, `{"4111111111111111":1}`}},
		{"custodians.settings", []string{`{"public_api_key":"public","account_updater":true,"account_updater_lookahead_days":"30"}`}, []string{`{"secret_api_key":"x"}`, `{"account_updater":"maybe"}`}},
		{"admission_operations.capture_terms", []string{`null`, `{"metadata":{}}`}, []string{`{"metadata":{"opaque":"replay-fact"}}`}},
		{"catalog_meters.group_by", []string{`null`, `{"region":"$.region"}`}, []string{`{"region":1}`}},
		{"catalog_rate_cards.filter", []string{`null`, `{"region":["us"]}`}, []string{`{"region":"us"}`}},
		{"payments.metadata", []string{
			`null`, `{}`, `{"order_id":"checkout-order","provider_transaction_id":"sale-1"}`,
			`{"stripe_invoice_id":"in_paid","e2e_run_id":"run-1"}`,
			`{"order_id":"payment:41111111-1111-4115-a111-111111111111"}`,
			`{"initial_payment_reversal":"refund"}`, `{"initial_payment_reversal":"dispute"}`,
		}, []string{
			`{"unknown":"value"}`, `{"security_key":"secret"}`, `{"order_id":{"raw":"body"}}`,
			`{"provider_transaction_id":"sk_test_secret"}`, `{"order_id":"4111111111111111"}`,
			`{"initial_payment_reversal":"refunded"}`, `{"initial_payment_reversal":true}`, `{"initial_payment_reversal":null}`,
		}},
		{"subscriptions.gateway_response", []string{
			`null`, `{}`, `{"initial_payment_reversal":"refund"}`,
			`{"order_id":"checkout-1","provider_transaction_id":"charge-1","delayed_start":"2027-01-01T00:00:00Z","e2e_run_id":"run-1","admin_notes":"billing note"}`,
			`{"order_id":"checkout-1","superseded_at":"2026-09-17T00:00:00Z","superseded_by_subscription_id":"10000000-0000-0000-0000-000000000001"}`,
			`{"previous_gateway_response":null,"superseded_at":"2026-09-17T00:00:00Z","superseded_by_subscription_id":null}`,
		}, []string{
			`{"order_id":"checkout-1","unknown":"value"}`, `{"raw_body":{"order_id":"checkout-1"}}`,
			`{"order_id":42}`, `{"provider_transaction_id":"sk_live_secret"}`,
			`{"previous_gateway_response":{"secret":"unsafe"}}`, `[]`, `{"initial_payment_reversal":{"raw":"body"}}`,
		}},
		{"no.such_field", nil, []string{`null`, `{}`}},
		{"payments.entitlements_spec_snapshot", nil, []string{strings.Repeat("[", 40) + strings.Repeat("]", 40)}},
	} {
		for _, raw := range tc.ok {
			if err := validateJSON(tc.field, raw); err != nil {
				t.Errorf("%s refused %s: %v", tc.field, raw, err)
			}
		}
		for _, raw := range tc.bad {
			if validateJSON(tc.field, raw) == nil {
				t.Errorf("%s accepted %s", tc.field, raw)
			}
		}
	}
}

func TestTextArraySpelling(t *testing.T) {
	for _, v := range []string{`{}`, `{USD,daily}`, `{"USD,daily","quoted\"key","back\\slash"}`} {
		if !safeTextArray(v) {
			t.Errorf("refused %s", v)
		}
	}
	for _, v := range []string{``, `x`, `[2:2]={x}`, `{{x}}`, `{NULL}`, `{null}`, `{x,}`, `{,x}`, `{x y}`, `{x,4111111111111111}`, `{sk_live_test}`, `{"unclosed}`, `{x\}`} {
		if safeTextArray(v) {
			t.Errorf("accepted %s", v)
		}
	}
}
