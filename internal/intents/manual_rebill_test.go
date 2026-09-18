package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func manualRebillIntent(t *testing.T, payload ManualRebillPayload) gen.OpenrailsRailIntent {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return gen.OpenrailsRailIntent{
		ID:             uuid.New(),
		IntentType:     TypeManualRebill,
		Rail:           "mobius",
		Payload:        raw,
		IdempotencyKey: "k",
		Origin:         string(OriginSystem),
		Attempts:       1,
		Status:         StatusInFlight,
	}
}

func testManualRebillPayload() ManualRebillPayload {
	subID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	return ManualRebillPayload{
		SubscriptionID:  subID,
		PeriodEnd:       time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Rail:            "mobius",
		OrderReference:  fmt.Sprintf("rebill-%s-%d", subID, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Unix()),
		Attempt:         1,
		PaymentMethodID: uuid.MustParse("66666666-7777-8888-9999-000000000000"),
		Instrument: RebillInstrument{
			PSPID: uuid.MustParse("77777777-7777-4777-8777-777777777777"), Custodian: models.CustodianPSP,
			RailCustomerRef: "vault-frozen", RailMethodRef: "billing-frozen",
		},
		Currency:    "USD",
		Amount:      12_000_000,
		AmountMinor: 1200,
	}
}

// TestManualRebillIdempotencyKey pins the identity contract: same content
// (sub + period + rail + order ref + attempt ordinal) -> same key (the
// crash-repair conflict), different attempt ordinal -> different key (each
// scheduled retry is a fresh intent).
func TestManualRebillIdempotencyKey(t *testing.T) {
	p := testManualRebillPayload()
	key := ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd, p.Rail, p.OrderReference, p.Attempt)
	assert.Equal(t, key, ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd, "MOBIUS", p.OrderReference, p.Attempt),
		"rail casing must not split identities")
	assert.NotEqual(t, key, ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd, p.Rail, p.OrderReference, p.Attempt+1),
		"the next scheduled retry is a new intent")
	assert.NotEqual(t, key, ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd.Add(30*24*time.Hour), p.Rail, p.OrderReference, p.Attempt),
		"another period is another charge")
}

func TestManualRebillExecuteParksBeforeProviderTraffic(t *testing.T) {
	intent := manualRebillIntent(t, testManualRebillPayload())

	t.Run("unarmed rail parks", func(t *testing.T) {
		h := NewManualRebillHandler(nil, nil, fakeNMIResolver{}, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "not armed")
	})

	t.Run("read-only client", func(t *testing.T) {
		client := newTestNMIClient(t, "")
		client.ReadOnly = true
		h := NewManualRebillHandler(nil, nil, fakeNMIResolver{client: client}, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "read-only")
	})
}

func TestManualRebillUnusablePayloadIsTerminal(t *testing.T) {
	h := NewManualRebillHandler(nil, nil, fakeNMIResolver{client: newTestNMIClient(t, "")}, nil)
	intent := manualRebillIntent(t, testManualRebillPayload())
	intent.Payload = []byte(`{}`)
	out := h.Execute(context.Background(), intent)
	assert.Equal(t, OutcomeTerminal, out.Class)
}

// rebillGateway answers the order search and the v5 exact read of one sale.
func rebillGateway(t *testing.T, searchXML string, exact map[string]any) (*nmi.NMIClient, *string) {
	t.Helper()
	var gotOrderID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/payments/") {
			if exact == nil {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND","message":"transaction not found"}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(exact)
			return
		}
		_ = r.ParseForm()
		gotOrderID = r.Form.Get("order_id")
		fmt.Fprint(w, searchXML)
	}))
	t.Cleanup(srv.Close)
	return newTestNMIClient(t, srv.URL), &gotOrderID
}

func exactSale(txn, vault, amount, currency string, success bool) map[string]any {
	response := "1"
	if !success {
		response = "2"
	}
	return map[string]any{
		"object": "transaction", "id": txn, "response": response, "response_code": "100",
		"amount": amount, "currency": currency, "customer_vault_id": vault,
		"actions": []map[string]any{{"id": txn, "type": "sale", "amount": amount, "success": success}},
	}
}

// TestManualRebillExactReceipt: the verifier's provider read is the ONE
// exact-receipt path — the order reference's successful sale, read back
// approved on the frozen vault for the frozen amount and currency. Declines
// and foreign order ids never count; a sale under the order reference that
// contradicts any frozen fact keeps the operation unknown with the
// contradiction retained, and nothing is finalized.
func TestManualRebillExactReceipt(t *testing.T) {
	p := testManualRebillPayload()
	sale := func(txn, success string) string {
		return fmt.Sprintf(`<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id>
			<action><action_type>sale</action_type><success>%s</success></action></transaction></nm_response>`, txn, p.OrderReference, success)
	}
	cases := []struct {
		name      string
		xml       string
		exact     map[string]any
		wantFound bool
		mismatch  bool
	}{
		{name: "exact sale", xml: sale("txn_ok", "1"), exact: exactSale("txn_ok", "vault-frozen", "12.00", "USD", true), wantFound: true},
		{name: "declined sale does not count", xml: sale("txn_declined", "0")},
		{name: "foreign order id ignored even if NMI over-returns", xml: `<nm_response><transaction><transaction_id>txn_other</transaction_id><order_id>rebill-other</order_id>
			<action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`},
		{name: "empty report", xml: `<nm_response></nm_response>`},
		{name: "another vault", xml: sale("txn_ok", "1"), exact: exactSale("txn_ok", "someone-elses-vault", "12.00", "USD", true), mismatch: true},
		{name: "another amount", xml: sale("txn_ok", "1"), exact: exactSale("txn_ok", "vault-frozen", "0.01", "USD", true), mismatch: true},
		{name: "another currency", xml: sale("txn_ok", "1"), exact: exactSale("txn_ok", "vault-frozen", "12.00", "EUR", true), mismatch: true},
		{name: "not approved", xml: sale("txn_ok", "1"), exact: exactSale("txn_ok", "vault-frozen", "12.00", "USD", false), mismatch: true},
		{name: "exact read missing", xml: sale("txn_ok", "1"), mismatch: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, gotOrderID := rebillGateway(t, tc.xml, tc.exact)
			txnID, found, err := client.ConfirmOrderSale(context.Background(), p.Receipt(), "")
			assert.Equal(t, p.OrderReference, *gotOrderID, "query must filter by the period's order reference")
			if tc.mismatch {
				require.ErrorIs(t, err, nmi.ErrReceiptMismatch)
				h := NewManualRebillHandler(nil, nil, fakeNMIResolver{client: client}, nil)
				out := h.Verify(context.Background(), manualRebillIntent(t, p))
				require.Equal(t, OutcomeAmbiguous, out.Class, "a contradicted receipt never settles")
				require.Contains(t, out.Evidence, rebillEvidenceContradiction)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantFound, found)
			if tc.wantFound {
				assert.Equal(t, "txn_ok", txnID)
			}
		})
	}
}

func TestManualRebillVerifyReadFailureStaysAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	h := NewManualRebillHandler(nil, nil, fakeNMIResolver{client: newTestNMIClient(t, srv.URL)}, nil)
	out := h.Verify(context.Background(), manualRebillIntent(t, testManualRebillPayload()))
	assert.Equal(t, OutcomeAmbiguous, out.Class)
}
