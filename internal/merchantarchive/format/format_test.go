package format

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const testMerchant = "10000000-0000-0000-0000-000000000001"

func emptyArchive(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := NewWriter(&b, testMerchant)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range Profiles {
		if err := w.Table(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestCopyVerifiedAndTruncation(t *testing.T) {
	good := emptyArchive(t)
	var out bytes.Buffer
	i, err := CopyVerified(&out, bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if i.MerchantID != testMerchant || i.Rows != 0 || len(i.Digest) != 64 || !bytes.Equal(out.Bytes(), good) {
		t.Fatal(i)
	}
	for _, n := range []int{0, 1, len(good) / 2, len(good) - 1, len(good) - 2} {
		if _, err := CopyVerified(io.Discard, bytes.NewReader(good[:n])); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	for _, bad := range [][]byte{
		append(append([]byte{}, good...), []byte("{}\n")...),
		bytes.Replace(good, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(good, []byte(`"consistency":"repeatable_read"`), []byte(`"consistency":"read_committed"`), 1),
		bytes.Replace(good, []byte(`"rows":0`), []byte(`"rows":1`), 1),
		bytes.Replace(good, []byte(`"kind":"header"`), []byte(`"kind":"header","kind":"header"`), 1),
		bytes.Replace(good, []byte(`"table":"customers"`), []byte(`"table":"merchant_secrets"`), 1),
	} {
		if _, err := CopyVerified(io.Discard, bytes.NewReader(bad)); err == nil {
			t.Fatal("accepted invalid archive")
		}
	}
}

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

func TestRecordBoundAndWriterOrder(t *testing.T) {
	if _, err := CopyVerified(io.Discard, strings.NewReader(strings.Repeat("x", MaxRecordBytes)+"\n")); err == nil {
		t.Fatal("accepted oversized line")
	}
	w, err := NewWriter(io.Discard, testMerchant)
	if err != nil {
		t.Fatal(err)
	}
	if w.Table(Profiles[1]) == nil {
		t.Fatal("accepted unordered table")
	}
	if w.Close() == nil {
		t.Fatal("accepted missing tables")
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
