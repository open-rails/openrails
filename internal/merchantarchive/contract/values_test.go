package contract

import (
	"strings"
	"testing"
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
		{"int32 max", "products", map[string]string{"tier_rank": "2147483647"}, true},
		{"int32 overflow", "products", map[string]string{"tier_rank": "2147483648"}, false},
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
		{"open checkout", "checkout_attempts", map[string]string{"status": "open"}, false},
		{"expired checkout", "checkout_attempts", map[string]string{"status": "expired"}, true},
		{"admitted admission", "admission_operations", map[string]string{"state": "admitted"}, false},
		{"captured admission", "admission_operations", map[string]string{"state": "captured"}, true},
		{"claimed intent", "provider_intents", map[string]string{"status": "claimed"}, false},
		{"unresolved intent", "provider_intents", map[string]string{"status": "unknown_needs_verify"}, false},
		{"superseded intent", "provider_intents", map[string]string{"status": "superseded"}, true},
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
		{"engine nmi null schedule", "subscriptions", map[string]string{"collection_policy": "engine", "rail": "nmi"}, true},
		{"engine stripe null schedule", "subscriptions", map[string]string{"collection_policy": "engine", "rail": "stripe"}, true},
		{"provider null schedule", "subscriptions", map[string]string{"collection_policy": "provider", "rail": "stripe"}, false},
		{"provider empty schedule", "subscriptions", sub("provider", "stripe", ""), false},
		{"engine nmi with provider schedule", "subscriptions", sub("engine", "nmi", "sub-1"), false},
		{"engine solana", "subscriptions", sub("engine", "solana", "pda"), true},
		{"engine solana null binding", "subscriptions", map[string]string{"collection_policy": "engine", "rail": "solana"}, false},
		{"engine ccbill", "subscriptions", sub("engine", "ccbill", ""), false},
		{"missing policy", "subscriptions", sub("", "nmi", ""), false},
		{"unknown policy", "subscriptions", sub("manual", "nmi", ""), false},
	})
}

func TestCatalogApplicationReceiptMatchesRow(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			"application_id": "deploy-1", "request_sha256": `\x` + strings.Repeat("ab", 32),
			"base_revision": "4", "applied_revision": "5", "applied_at": "2026-01-01 00:00:00+00",
			"result": `{"application_id":"deploy-1","base_revision":4,"applied_revision":5,"replayed":false,"products_changed":1,"prices_changed":0}`,
		}
	}
	cases := []rowCase{{"exact receipt", "catalog_applications", base(), true}}
	for name, mutate := range map[string]func(map[string]string){
		"other application": func(m map[string]string) { m["application_id"] = "deploy-2" },
		"skipped revision":  func(m map[string]string) { m["applied_revision"] = "6" },
		"short digest":      func(m map[string]string) { m["request_sha256"] = `\xab` },
		"non-hex digest":    func(m map[string]string) { m["request_sha256"] = `\x` + strings.Repeat("zz", 32) },
		"replayed receipt": func(m map[string]string) {
			m["result"] = strings.Replace(m["result"], `"replayed":false`, `"replayed":true`, 1)
		},
		"negative change": func(m map[string]string) {
			m["result"] = strings.Replace(m["result"], `"prices_changed":0`, `"prices_changed":-1`, 1)
		},
		"missing result": func(m map[string]string) { delete(m, "result") },
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
		{"products.entitlements", []string{`[]`, `["premium","secret_content"]`}, []string{`null`, `{"premium":null}`, `["4111111111111111"]`}},
		{"custodians.settings", []string{`{"public_api_key":"public","account_updater":true,"account_updater_lookahead_days":"30"}`}, []string{`{"secret_api_key":"x"}`, `{"account_updater":"maybe"}`}},
		{"admission_operations.capture_terms", []string{`null`, `{"metadata":{"opaque":"replay-fact"}}`}, []string{`{"unknown_operation_field":true}`}},
		{"catalog_meters.group_by", []string{`null`, `{"region":"$.region"}`}, []string{`{"region":1}`}},
		{"catalog_rate_cards.filter", []string{`null`, `{"region":["us"]}`}, []string{`{"region":"us"}`}},
		{"no.such_field", nil, []string{`null`, `{}`}},
		{"payments.entitlements_snapshot", nil, []string{strings.Repeat("[", 40) + strings.Repeat("]", 40)}},
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

// Metadata belongs to the application. Archive portability cannot depend on
// the keys, nesting or strings an application happens to store in its JSON.
func TestApplicationMetadata(t *testing.T) {
	fields := []string{
		"payments.metadata", "payments.discount_metadata", "payment_methods.metadata",
		"usage_events.metadata", "invoice_items.metadata", "checkout_attempts.metadata",
		"subscriptions.gateway_response",
	}
	valid := []string{
		`null`, `{}`, `[]`, `"application note"`, `9007199254740993`, `true`,
		`{"period_start":"2026-10-07T00:00:00Z","solana":{"signature":"transfer-1","amount_base_units":9007199254740993},"application":{"nested":[null,true,1.25,{"new_key":"value"}]}}`,
		`{"security_key":"application field name","campaign":"sk_test_campaign","note":"-----BEGIN APPLICATION NOTE-----","order_number":"4111111111111111"}`,
	}
	invalid := []string{`{"unfinished":`, `{"key":1,"key":2}`, `{} {}`, strings.Repeat("[", 34) + strings.Repeat("]", 34)}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			for _, raw := range valid {
				if err := validateJSON(field, raw); err != nil {
					t.Errorf("application metadata refused: %s: %v", raw, err)
				}
			}
			for _, raw := range invalid {
				if err := validateJSON(field, raw); err == nil {
					t.Errorf("invalid or unbounded JSON accepted: %s", raw)
				}
			}
		})
	}
	for _, raw := range valid {
		if err := validateJSON("admission_operations.capture_terms", `{"metadata":`+raw+`}`); err != nil {
			t.Errorf("capture metadata refused: %s: %v", raw, err)
		}
	}
	if err := validateJSON("admission_operations.capture_terms", `{"unknown_operation_field":true}`); err == nil {
		t.Fatal("application metadata allowance changed the outer operation contract")
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

func TestPurchasedCreditSnapshotRequiresFulfillmentDates(t *testing.T) {
	for _, tc := range []struct {
		snapshot string
		valid    bool
	}{
		{`{"amount":"120000000","currency":"USD","expires_after_days":365,"starts_at":"2026-01-01T00:00:00Z","expires_at":"2027-01-01T00:00:00Z"}`, true},
		{`{"amount":"120000000","currency":"USD","expires_after_days":365}`, false},
		{`{"amount":"120000000","currency":"USD","expires_after_days":365,"starts_at":"2026-01-01T00:00:00Z","expires_at":"2026-02-01T00:00:00Z"}`, false},
		{`{"amount":"120000000","currency":"EUR","expires_after_days":365,"starts_at":"2026-01-01T00:00:00Z","expires_at":"2027-01-01T00:00:00Z"}`, false},
	} {
		p, v := row(t, "payments", map[string]string{"currency": "USD", "status": "completed", "credit_grant_snapshot": tc.snapshot})
		err := ValidateValues(p, v)
		if (err == nil) != tc.valid {
			t.Errorf("snapshot %s valid=%v: %v", tc.snapshot, tc.valid, err)
		}
	}
}
