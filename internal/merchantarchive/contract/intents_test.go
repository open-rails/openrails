package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/pkg/pricing"
)

func TestAcceptedPurchaseMustMatchCheckoutAndBeResolved(t *testing.T) {
	var cases []rowCase
	for _, tc := range []struct {
		name, status, amount, rail string
		submitted, closed, valid   bool
	}{
		{"paid", "succeeded", "100", "stripe", true, false, true},
		{"stripe local expiry is unknown", "expired", "100", "stripe", true, false, false},
		{"stripe provider closed", "expired", "100", "stripe", true, true, true},
		{"never submitted", "failed", "100", "stripe", false, true, true},
		{"changed amount", "succeeded", "101", "stripe", true, false, false},
		{"negative amount", "succeeded", "-100", "stripe", true, false, false},
	} {
		state := fmt.Sprintf(`{"accepted_purchase":{"price_id":%q,"product_id":%q,"payment_id":%q,"product_key":"post","product_name":"Post","amount":%q,"currency":"USD","access_duration_hours":null,"entitlements":{"post:one":null},"accepted_at":"2026-09-23T00:00:00Z","entitlement_start":"2026-09-23T00:00:00Z"},"purchase_submitted":%t,"provider_closed":%t}`,
			testMerchant, testMerchant, testMerchant, tc.amount, tc.submitted, tc.closed)
		cases = append(cases, rowCase{tc.name, "checkout_sessions", map[string]string{
			"mode": "one_off", "rail": tc.rail, "status": tc.status, "price_id": testMerchant, "amount": "100", "currency": "USD", "rail_state": state,
		}, tc.valid})
	}
	checkRows(t, cases)
}

func TestCheckoutRailStateContracts(t *testing.T) {
	// A real NMI checkout fingerprint; it contains a Luhn-valid 16-digit run.
	const digest = "cf0b56594a544b069795b7521f553ad1ab9dbc4413daf5369343142307519d66"
	if safeText(digest) {
		t.Fatal("fixture must exercise the PAN-shaped substring")
	}
	for raw, valid := range map[string]bool{
		`{"_openrails_request_fingerprint":"` + digest + `"}`:                              true,
		`{"_openrails_request_fingerprint":"4111111111111111"}`:                            false,
		`{"_openrails_request_fingerprint":"not-a-sha256"}`:                                false,
		`{"_openrails_request_fingerprint":"` + strings.ToUpper(digest) + `"}`:             false,
		`{"_openrails_request_fingerprint":42}`:                                            false,
		`{"_openrails_request_fingerprint":"` + digest + `","message":"4111111111111111"}`: false,
		`{"_openrails_request_fingerprint":"` + digest + `","unknown":"retained"}`:         false,
	} {
		if err := validateJSON("checkout_sessions.rail_state", raw); (err == nil) != valid {
			t.Errorf("%s: valid=%v, error=%v", raw, valid, err)
		}
	}
	const session = `"12345678-1234-4234-8234-123456789012"`
	for _, raw := range []string{session, `"checkout_session:12345678-1234-4234-8234-123456789012"`, `"bad"`, `null`, `true`, `{}`} {
		err := validateJSON("rail_intents.initial_membership.payload", `{"checkout_session_id":`+raw+`}`)
		if (err == nil) != (raw == session) {
			t.Errorf("session binding %s: %v", raw, err)
		}
	}
}

func TestRetainedIntentsNeedQualifiedEvidence(t *testing.T) {
	var cases []rowCase
	for _, tc := range []struct{ name, typ, status, payload, evidence string }{
		{"pruned sale has no accepted terms", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","transaction_id":"sale-1"}`},
		{"pruned subscription has no accepted terms", "initial_membership", "succeeded", "{}", `{"subscription_id":"` + testMerchant + `","transaction_id":"sale-1","status":"success","message":"Subscription created successfully"}`},
		{"boolean marker is not sale custody", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","transaction_id":"sale-1","verified_existing":true}`},
		{"unknown result", "nmi_sale", "succeeded", "null", `{"payment_id":"` + testMerchant + `","raw_body":{}}`},
		{"credential result", "initial_membership", "succeeded", "null", `{"security_key":"secret"}`},
		{"credential in known field", "nmi_sale", "succeeded", "null", `{"transaction_id":"sk_live_secret"}`},
		{"unqualified sale payload", "nmi_sale", "failed_terminal", `{"customer_vault_id":"vault-1"}`, `{}`},
		{"ephemeral subscription token", "initial_membership", "failed_terminal", `{"payment_token":"token"}`, `{}`},
		{"succeeded sale cannot carry payload", "nmi_sale", "succeeded", `{"original_payment_id":"` + testMerchant + `"}`, `{}`},
		{"wrong id type", "nmi_sale", "succeeded", "null", `{"payment_id":42}`},
		{"wrong code type", "initial_membership", "failed_terminal", "null", `{"response_code":"100"}`},
		{"malformed id", "initial_membership", "succeeded", "null", `{"subscription_id":"not-a-uuid"}`},
		{"refund with unqualified evidence", "nmi_refund", "succeeded", "null", `{"payment_id":"` + testMerchant + `"}`},
		{"unsupported type with payload", "ccbill_cancel", "succeeded", `{"reason":"x"}`, `null`},
	} {
		cases = append(cases, rowCase{tc.name, "rail_intents", map[string]string{"intent_type": tc.typ, "status": tc.status, "payload": tc.payload, "result_evidence": tc.evidence}, false})
	}
	checkRows(t, cases)
}

func TestHostSettlementDedupeKeyNamesItsPayment(t *testing.T) {
	// The first 16 digits pass Luhn, but this is the canonical UUID written by
	// enqueue_payment_settlement_event; only the coordinate check confines it.
	payment := "41111111-1111-4115-a111-111111111111"
	event := func(typ, pay, key string) map[string]string {
		return map[string]string{"event_type": typ, "payment_id": pay, "dedupe_key": key, "delivered_at": "2026-09-18 00:00:00+00"}
	}
	cases := []rowCase{
		{"luhn-looking uuid key", "host_outbox", event("payment.settled", payment, "payment:"+payment), true},
		{"ordinary key", "host_outbox", event("payment.settled", testMerchant, "payment:"+testMerchant), true},
		{"payment key on another event", "host_outbox", event("delinquency.entered", payment, "payment:"+payment), false},
		{"settlement without key", "host_outbox", event("payment.settled", payment, "unrelated-safe-key"), false},
	}
	for _, bad := range []string{"4111111111111111", "payment:4111111111111111", "payment:" + payment + ":4111111111111111", "payment:41111111-1111-4115-a111-111111111112", "payment:" + testMerchant} {
		cases = append(cases, rowCase{"key " + bad, "host_outbox", event("payment.settled", payment, bad), false})
	}
	checkRows(t, cases)
}

const collectionPayload = `{"initiator":"%s","invoice_id":"10000000-0000-0000-0000-000000000001","customer_id":"10000000-0000-0000-0000-000000000002","attempt_id":"10000000-0000-0000-0000-000000000003","payment_method_id":"10000000-0000-0000-0000-000000000004","rail":"nmi","currency":"USD","amount":50000,"amount_minor":5,"description":"invoice","instrument":{"psp_id":"10000000-0000-0000-0000-000000000005","custodian":"psp","rail_customer_ref":"vault-original","rail_method_ref":"billing-original"}}`

func collectionIntent(t *testing.T, initiator, origin, actor, key string) map[string]string {
	t.Helper()
	payload := fmt.Sprintf(collectionPayload, initiator)
	var decoded any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(decoded)
	digest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	evidence := `{"qualified_receipt":{"version":1,"family":"collected_payment","binding":{"operation_id":"10000000-0000-0000-0000-000000000006","merchant_id":"10000000-0000-0000-0000-000000000007","psp_id":"10000000-0000-0000-0000-000000000005","kind":"invoice_collection","payload_sha256":"` + digest + `"},"nmi":{"transaction_id":"sale-original","order_reference":"10000000-0000-0000-0000-000000000006","customer_vault_id":"vault-original","amount":"5","currency":"USD","approved":true}}}`
	m := map[string]string{"id": "10000000-0000-0000-0000-000000000006", "merchant_id": "10000000-0000-0000-0000-000000000007", "psp_id": "10000000-0000-0000-0000-000000000005", "rail": "nmi", "intent_type": "invoice_collection", "status": "succeeded", "origin": origin, "idempotency_key": key, "payload": payload, "result_evidence": evidence}
	if actor != "" {
		m["actor"] = actor
	}
	return m
}

// Engine-generated collection keys are hash-shaped and may contain Luhn runs.
// They are exempt from the text scan only after the payer, payload digest and
// receipt all bind them; any other hash-looking text is still scanned.
func TestCollectionKeysAreBoundIdentities(t *testing.T) {
	payer := uuid.MustParse("10000000-0000-0000-0000-000000000002")
	invoice := uuid.MustParse("10000000-0000-0000-0000-000000000001")
	customerKey := charge.CustomerPaymentKey("invoice_collection", payer, "archive-key-1461")
	retryKey := intents.InvoiceCollectionRetryKey(invoice, "admin-archive-3817")
	if safeText(customerKey) || safeText(retryKey) {
		t.Fatal("fixtures must be keys the text scan alone would refuse")
	}
	customer := func() map[string]string { return collectionIntent(t, "customer", "user", payer.String(), customerKey) }
	merchant := func() map[string]string { return collectionIntent(t, "merchant", "admin", "", retryKey) }
	cases := []rowCase{
		{"customer key", "rail_intents", customer(), true},
		{"merchant retry key", "rail_intents", merchant(), true},
	}
	for name, tc := range map[string]struct {
		base   func() map[string]string
		field  string
		mutate func(string) string
	}{
		"caller PAN key":            {customer, "idempotency_key", func(string) string { return "4111111111111111" }},
		"uppercase key":             {customer, "idempotency_key", strings.ToUpper},
		"wrong payer":               {customer, "actor", func(string) string { return testMerchant }},
		"wrong origin":              {customer, "origin", func(string) string { return "admin" }},
		"wrong digest":              {customer, "result_evidence", func(e string) string { return replaceDigest(e, strings.Repeat("0", 64)) }},
		"malformed digest":          {customer, "result_evidence", func(e string) string { return replaceDigest(e, "not-a-hash") }},
		"PAN in evidence":           {customer, "result_evidence", func(e string) string { return strings.ReplaceAll(e, "sale-original", "4111111111111111") }},
		"succeeded without receipt": {customer, "result_evidence", func(string) string { return "null" }},
		"retry key for other invoice": {merchant, "idempotency_key", func(k string) string {
			return strings.Replace(k, invoice.String(), "10000000-0000-0000-0000-000000000008", 1)
		}},
		"retry key of other kind":    {merchant, "idempotency_key", func(k string) string { return strings.Replace(k, "invoice_collection:", "manual_rebill:", 1) }},
		"retry key from user origin": {merchant, "origin", func(string) string { return "user" }},
		"unsupported payload field":  {customer, "payload", func(p string) string { return strings.Replace(p, `"description":"invoice"`, `"provider_body":{}`, 1) }},
		"instrument credential":      {customer, "payload", func(p string) string { return strings.Replace(p, `"custodian":"psp"`, `"security_key":"secret"`, 1) }},
		"instrument non-uuid psp": {customer, "payload", func(p string) string {
			return strings.Replace(p, `"instrument":{"psp_id":"10000000-0000-0000-0000-000000000005"`, `"instrument":{"psp_id":"not-a-uuid"`, 1)
		}},
	} {
		m := tc.base()
		m[tc.field] = tc.mutate(m[tc.field])
		cases = append(cases, rowCase{name, "rail_intents", m, false})
	}
	// The key is exempt only in its own column.
	leaked := customer()
	leaked["origin_reason"] = customerKey
	cases = append(cases, rowCase{"key in ordinary text", "rail_intents", leaked, false})
	checkRows(t, cases)

	resolved := `{"transaction_id":"sale-original","operator_resolution":{"actor":"operator","reason":"confirmed exact provider receipt","resolved_at":"2026-09-18T00:00:00Z","provider_reference":"sale-original"}}`
	if err := validateJSON("rail_intents.invoice_collection.result_evidence", resolved); err != nil {
		t.Fatalf("operator resolution evidence must be retained verbatim: %v", err)
	}
	if validateJSON("rail_intents.invoice_collection.result_evidence", `{"operator_resolution":{"actor":"operator","raw_provider_body":{}}}`) == nil {
		t.Fatal("accepted raw operator evidence")
	}
}

func replaceDigest(evidence, digest string) string {
	i := strings.Index(evidence, `"payload_sha256":"`) + len(`"payload_sha256":"`)
	return evidence[:i] + digest + evidence[i+64:]
}

func TestRateCardPriceArchiveMatchesPersistedShape(t *testing.T) {
	ceiling := int64(10)
	for _, price := range []pricing.RatePrice{
		{Model: pricing.ModelFlat, Currency: "USD", Flat: &pricing.FlatPrice{Amount: math.MaxInt64}},
		{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 9007199254740993, DivideBy: 60, MaximumAmount: math.MaxInt64}},
		{Model: pricing.ModelPerUnit, Currency: "USD", PerUnit: &pricing.PerUnitPrice{DivideBy: 60, Round: pricing.RoundUp, Matrix: &pricing.Matrix{Dimension: "size", Cells: map[string]pricing.MatrixCell{"small": {}, "large": {UnitAmount: 9007199254740993, MaximumAmount: math.MaxInt64, Included: 10}}}}},
		{Model: pricing.ModelTiered, Currency: "USD", Tiered: &pricing.TieredPrice{Mode: pricing.TierModeGraduated, Tiers: []pricing.RateTier{{UpTo: &ceiling, UnitAmount: 100, FlatAmount: 9007199254740993}, {UnitAmount: 50}}}},
		{Model: pricing.ModelPackage, Currency: "USD", Package: &pricing.PackagePrice{Amount: 9007199254740993, PackageSize: 100, FreeUnits: 10}},
	} {
		raw, err := json.Marshal(price)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateJSON("catalog_rate_cards.price", string(raw)); err != nil {
			t.Errorf("persisted price refused: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"flat":{"amount":2}}`, `{"per_unit":{"unit_amount":2}}`, `{"per_unit":{"maximum_amount":2}}`,
		`{"per_unit":{"matrix":{"dimension":"size","cells":{"large":{"unit_amount":2}}}}}`,
		`{"tiered":{"tiers":[{"unit_amount":2,"flat_amount":"1"}]}}`, `{"package":{"amount":2}}`,
		`{"flat":{"amount":"9223372036854775808"}}`, `{"flat":{"amount":"1e3"}}`, `{"flat":{"amount":"+1"}}`,
		`{"per_unit":{"unit_amount":"2","security_key":"secret"}}`,
		`{"per_unit":{"matrix":{"dimension":"size","cells":{"large":{"unit_amount":"2","card_number":"4111111111111111"}}}}}`,
		`{"maximum_amount":"2"}`, `{"matrix":{"dimension":"size","cells":{"large":{"unit_amount":"2"}}}}`,
	} {
		if validateJSON("catalog_rate_cards.price", raw) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

// Wire integrity (archivewire) never implies the billing contract holds.
func TestReadAppliesBillingContractOverIntactWire(t *testing.T) {
	var tables []string
	for _, p := range Profiles {
		tables = append(tables, p.Name)
	}
	mid, customer := testMerchant, "10000000-0000-0000-0000-000000000002"
	issuer, ts, unsafe := "https://merchant.example", "2026-09-17 12:00:00+00", "4111111111111111"
	for _, tc := range []struct {
		name   string
		tables []string
		row    []*string
		valid  bool
	}{
		{"supported", tables, []*string{&mid, &customer, &issuer, &ts, &ts}, true},
		{"unknown table", []string{"merchant_secrets"}, nil, false},
		{"missing tables", tables[:len(tables)-1], nil, false},
		{"wrong order", append([]string{tables[1], tables[0]}, tables[2:]...), nil, false},
		{"extra table", append(append([]string{}, tables...), tables[0]), nil, false},
		{"wrong row width", tables, []*string{&mid}, false},
		{"unsafe value", tables, []*string{&mid, &customer, &unsafe, &ts, &ts}, false},
	} {
		var artifact bytes.Buffer
		w, err := archivewire.NewWriter(&artifact, mid)
		if err != nil {
			t.Fatal(err)
		}
		for i, table := range tc.tables {
			if err := w.Table(table); err != nil {
				t.Fatal(err)
			}
			if i == 0 && tc.row != nil {
				if err := w.Row(tc.row); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := archivewire.CopyVerified(io.Discard, bytes.NewReader(artifact.Bytes())); err != nil {
			t.Fatalf("%s: wire integrity: %v", tc.name, err)
		}
		var rows int
		_, err = Read(bytes.NewReader(artifact.Bytes()), nil, func(Profile, []*string) error { rows++; return nil })
		if (err == nil) != tc.valid || tc.valid && rows != 1 {
			t.Errorf("%s: accepted=%t rows=%d, want %t: %v", tc.name, err == nil, rows, tc.valid, err)
		}
	}
}
