package contract

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestInvoiceCollectionArchiveContract(t *testing.T) {
	payload := `{"invoice_id":"10000000-0000-0000-0000-000000000001","customer_id":"10000000-0000-0000-0000-000000000002","attempt_id":"10000000-0000-0000-0000-000000000003","payment_method_id":"10000000-0000-0000-0000-000000000004","rail":"nmi","currency":"USD","amount":50000,"amount_minor":5,"description":"invoice","instrument":{"psp_id":"10000000-0000-0000-0000-000000000005","custodian":"psp","rail_customer_ref":"vault-original","rail_method_ref":"billing-original"}}`
	if err := validateJSON("rail_intents.invoice_collection.payload", payload); err != nil {
		t.Fatal(err)
	}
	// A completed operator resolution may retain its full evidence when the
	// optional post-success slimming step did not run. Preserve it verbatim.
	resolved := `{"transaction_id":"sale-original","operator_resolution":{"actor":"operator","reason":"confirmed exact provider receipt","resolved_at":"2026-09-18T00:00:00Z","provider_reference":"sale-original"}}`
	if err := validateJSON("rail_intents.invoice_collection.result_evidence", resolved); err != nil {
		t.Fatal(err)
	}
	if err := validateJSON("rail_intents.invoice_collection.result_evidence", `{"operator_resolution":{"actor":"operator","raw_provider_body":{}}}`); err == nil {
		t.Fatal("accepted unknown raw operator evidence")
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

func TestCollectedIdentityDigestsRemainCanonicalAndTextStillScanned(t *testing.T) {
	// Both hashes contain Luhn-looking runs. They are encoded coordinates, not
	// arbitrary text; the receipt's digest is recomputed from accepted payload.
	payload := `{"initiator":"customer","invoice_id":"10000000-0000-0000-0000-000000000001","customer_id":"10000000-0000-0000-0000-000000000002","attempt_id":"10000000-0000-0000-0000-000000000003","payment_method_id":"10000000-0000-0000-0000-000000000004","rail":"nmi","currency":"USD","amount":50000,"amount_minor":5,"description":"invoice-985","instrument":{"psp_id":"10000000-0000-0000-0000-000000000005","custodian":"psp","rail_customer_ref":"vault-original","rail_method_ref":"billing-original"}}`
	payer := uuid.MustParse("10000000-0000-0000-0000-000000000002")
	key := charge.CustomerPaymentKey("invoice_collection", payer, "archive-key-1461")
	digest := "5c6b8100ee05fca80d1732f7a94aba7d17c4038246781262883a0086eafc6852"
	require.False(t, safeText(key))
	require.False(t, safeText(digest))
	evidence := `{"qualified_receipt":{"version":1,"family":"collected_payment","binding":{"operation_id":"10000000-0000-0000-0000-000000000006","merchant_id":"10000000-0000-0000-0000-000000000007","psp_id":"10000000-0000-0000-0000-000000000005","kind":"invoice_collection","payload_sha256":"` + digest + `"},"nmi":{"transaction_id":"sale-original","order_reference":"10000000-0000-0000-0000-000000000006","customer_vault_id":"vault-original","amount":"5","currency":"USD","approved":true}}}`
	fields := map[string]string{"id": "10000000-0000-0000-0000-000000000006", "merchant_id": "10000000-0000-0000-0000-000000000007", "psp_id": "10000000-0000-0000-0000-000000000005", "rail": "nmi", "intent_type": "invoice_collection", "status": "succeeded", "origin": "user", "actor": payer.String(), "idempotency_key": key, "payload": payload, "result_evidence": evidence}
	var profile Profile
	for _, p := range Profiles {
		if p.Name == "rail_intents" {
			profile = p
		}
	}
	validate := func() error {
		values := make([]*string, len(profile.Columns))
		for i, c := range profile.Columns {
			if v, ok := fields[c.Name]; ok {
				values[i] = &v
			}
		}
		return ValidateValues(profile, values)
	}
	require.NoError(t, validate())
	for _, tc := range []struct{ name, field, bad string }{
		{"caller PAN key", "idempotency_key", "4111111111111111"},
		{"wrong payer", "actor", testMerchant},
		{"wrong origin", "origin", "admin"},
		{"uppercase key", "idempotency_key", strings.ToUpper(key)},
		{"wrong digest", "result_evidence", strings.ReplaceAll(evidence, digest, strings.Repeat("0", 64))},
		{"malformed digest", "result_evidence", strings.ReplaceAll(evidence, digest, "not-a-hash")},
		{"PAN in ordinary evidence", "result_evidence", strings.ReplaceAll(evidence, "sale-original", "4111111111111111")},
		{"hash in ordinary evidence", "result_evidence", strings.ReplaceAll(evidence, "sale-original", digest)},
		{"key in ordinary text", "origin_reason", key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, existed := fields[tc.field]
			fields[tc.field] = tc.bad
			require.Error(t, validate())
			if existed {
				fields[tc.field] = original
			} else {
				delete(fields, tc.field)
			}
		})
	}
}

func TestMerchantRetryKeyIsAnInvoiceBoundEncodedIdentity(t *testing.T) {
	invoice := uuid.MustParse("10000000-0000-0000-0000-000000000001")
	key := intents.InvoiceCollectionRetryKey(invoice, "admin-archive-3817")
	require.False(t, safeText(key), "deterministic generated-key collision")
	payload := `{"initiator":"merchant","invoice_id":"10000000-0000-0000-0000-000000000001","customer_id":"10000000-0000-0000-0000-000000000002","attempt_id":"10000000-0000-0000-0000-000000000003","payment_method_id":"10000000-0000-0000-0000-000000000004","rail":"nmi","currency":"USD","amount":50000,"amount_minor":5,"description":"invoice","instrument":{"psp_id":"10000000-0000-0000-0000-000000000005","custodian":"psp","rail_customer_ref":"vault-original","rail_method_ref":"billing-original"}}`
	var decoded any
	require.NoError(t, json.Unmarshal([]byte(payload), &decoded))
	canonical, err := json.Marshal(decoded)
	require.NoError(t, err)
	digest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	evidence := `{"qualified_receipt":{"version":1,"family":"collected_payment","binding":{"operation_id":"10000000-0000-0000-0000-000000000006","merchant_id":"10000000-0000-0000-0000-000000000007","psp_id":"10000000-0000-0000-0000-000000000005","kind":"invoice_collection","payload_sha256":"` + digest + `"},"nmi":{"transaction_id":"sale-original","order_reference":"10000000-0000-0000-0000-000000000006","customer_vault_id":"vault-original","amount":"5","currency":"USD","approved":true}}}`
	p := Profile{Name: "rail_intents", Columns: []Column{{"id", "uuid"}, {"merchant_id", "uuid"}, {"psp_id", "uuid"}, {"rail", "text"}, {"intent_type", "text"}, {"status", "text"}, {"origin", "text"}, {"payload", "jsonb"}, {"result_evidence", "jsonb"}, {"idempotency_key", "text"}}}
	original := []string{"10000000-0000-0000-0000-000000000006", "10000000-0000-0000-0000-000000000007", "10000000-0000-0000-0000-000000000005", "nmi", "invoice_collection", "succeeded", "admin", payload, evidence, key}
	for _, tc := range []struct {
		name, key string
		valid     bool
	}{
		{"generated", key, true},
		{"wrong invoice", strings.Replace(key, invoice.String(), "10000000-0000-0000-0000-000000000008", 1), false},
		{"wrong kind", strings.Replace(key, "invoice_collection:", "manual_rebill:", 1), false},
		{"ordinary raw key", "4111111111111111", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := make([]*string, len(original))
			for i, v := range original {
				values[i] = &v
			}
			values[len(values)-1] = &tc.key
			err := ValidateValues(p, values)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
