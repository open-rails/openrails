package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func manualRebillIntent(t *testing.T, payload ManualRebillPayload) gen.OpenrailsProviderIntent {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return gen.OpenrailsProviderIntent{
		ID:             uuid.New(),
		IntentType:     TypeManualRebill,
		Provider:       "mobius",
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
		SubscriptionID: subID,
		PeriodEnd:      time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Processor:      "mobius",
		OrderReference: fmt.Sprintf("rebill-%s-%d", subID, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Unix()),
		Attempt:        1,
	}
}

// TestManualRebillIdempotencyKey pins the identity contract: same content
// (sub + period + processor + order ref + attempt ordinal) -> same key (the
// crash-repair conflict), different attempt ordinal -> different key (each
// scheduled retry is a fresh intent).
func TestManualRebillIdempotencyKey(t *testing.T) {
	p := testManualRebillPayload()
	key := ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd, p.Processor, p.OrderReference, p.Attempt)
	assert.Equal(t, key, ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd, "MOBIUS", p.OrderReference, p.Attempt),
		"processor casing must not split identities")
	assert.NotEqual(t, key, ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd, p.Processor, p.OrderReference, p.Attempt+1),
		"the next scheduled retry is a new intent")
	assert.NotEqual(t, key, ManualRebillIdempotencyKey(p.SubscriptionID, p.PeriodEnd.Add(30*24*time.Hour), p.Processor, p.OrderReference, p.Attempt),
		"another period is another charge")
}

func TestManualRebillExecuteParksBeforeProviderTraffic(t *testing.T) {
	intent := manualRebillIntent(t, testManualRebillPayload())

	t.Run("no client for provider", func(t *testing.T) {
		h := NewManualRebillHandler(nil, nil, map[string]*nmi.NMIClient{}, nil, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "not configured")
	})

	t.Run("read-only client", func(t *testing.T) {
		client := newTestNMIClient(t, "")
		client.ReadOnly = true
		h := NewManualRebillHandler(nil, nil, map[string]*nmi.NMIClient{"mobius": client}, nil, nil)
		out := h.Execute(context.Background(), intent)
		assert.Equal(t, OutcomeParked, out.Class)
		assert.Contains(t, out.Reason, "read-only")
	})
}

func TestManualRebillUnusablePayloadIsTerminal(t *testing.T) {
	h := NewManualRebillHandler(nil, nil, map[string]*nmi.NMIClient{"mobius": newTestNMIClient(t, "")}, nil, nil)
	intent := manualRebillIntent(t, testManualRebillPayload())
	intent.Payload = []byte(`{}`)
	out := h.Execute(context.Background(), intent)
	assert.Equal(t, OutcomeTerminal, out.Class)
}

// TestManualRebillFindSuccessfulSale: the verifier read keys on the period's
// order reference — any successful sale for it means the period was charged;
// declines and foreign order ids never count.
func TestManualRebillFindSuccessfulSale(t *testing.T) {
	p := testManualRebillPayload()
	cases := []struct {
		name      string
		xml       string
		wantFound bool
		wantTxnID string
	}{
		{
			name: "successful sale found",
			xml: fmt.Sprintf(`<nm_response><transaction><transaction_id>txn_ok</transaction_id><order_id>%s</order_id>
				<action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, p.OrderReference),
			wantFound: true,
			wantTxnID: "txn_ok",
		},
		{
			name: "declined sale does not count",
			xml: fmt.Sprintf(`<nm_response><transaction><transaction_id>txn_declined</transaction_id><order_id>%s</order_id>
				<action><action_type>sale</action_type><success>0</success></action></transaction></nm_response>`, p.OrderReference),
			wantFound: false,
		},
		{
			name: "foreign order id ignored even if NMI over-returns",
			xml: `<nm_response><transaction><transaction_id>txn_other</transaction_id><order_id>rebill-other</order_id>
				<action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`,
			wantFound: false,
		},
		{
			name:      "empty report",
			xml:       `<nm_response></nm_response>`,
			wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotOrderID string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				gotOrderID = r.Form.Get("order_id")
				fmt.Fprint(w, tc.xml)
			}))
			t.Cleanup(srv.Close)
			h := NewManualRebillHandler(nil, nil, map[string]*nmi.NMIClient{"mobius": newTestNMIClient(t, srv.URL)}, nil, nil)

			txnID, found, err := h.findSuccessfulSale(h.Clients["mobius"], p)
			require.NoError(t, err)
			assert.Equal(t, p.OrderReference, gotOrderID, "query must filter by the period's order reference")
			assert.Equal(t, tc.wantFound, found)
			if tc.wantFound {
				assert.Equal(t, tc.wantTxnID, txnID)
			}
		})
	}
}

func TestManualRebillVerifyReadFailureStaysAmbiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	h := NewManualRebillHandler(nil, nil, map[string]*nmi.NMIClient{"mobius": newTestNMIClient(t, srv.URL)}, nil, nil)
	out := h.Verify(context.Background(), manualRebillIntent(t, testManualRebillPayload()))
	assert.Equal(t, OutcomeAmbiguous, out.Class)
}
