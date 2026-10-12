package stripemock

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func readInvoiceFixture(t *testing.T, m *Mock, path string) Object {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.stripe.com"+path, nil)
	require.NoError(t, err)
	response, err := m.RoundTrip(r)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var out Object
	require.NoError(t, json.NewDecoder(response.Body).Decode(&out))
	return out
}

func TestInvoiceHistoryKeepsFrozenBillsAndMoney(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	m := NewUnstarted(Options{Clock: func() time.Time { return now }})
	sub := m.AddSubscription("cus_bills", "pm_bills", "price_legacy_month", 999, now, now.Add(720*time.Hour))
	first := m.Subscription(sub)["latest_invoice"].(Object)
	m.RenewSubscription(sub, true)
	m.RenewSubscription(sub, false)
	m.AddSubscription("cus_other", "pm_other", "price_legacy_month", 999, now, now.Add(720*time.Hour))
	page := readInvoiceFixture(t, m, "/v1/invoices?subscription="+sub+"&limit=2")
	require.Equal(t, true, page["has_more"])
	rows := page["data"].([]any)
	require.Len(t, rows, 2)
	next := readInvoiceFixture(t, m, "/v1/invoices?subscription="+sub+"&limit=2&starting_after="+rows[1].(Object)["id"].(string))
	require.Equal(t, false, next["has_more"])
	require.Len(t, next["data"], 1)
	id := first["id"].(string)
	require.Equal(t, first, readInvoiceFixture(t, m, "/v1/invoices/"+id), "later billing never rewrites a finalized invoice")
	lines := readInvoiceFixture(t, m, "/v1/invoices/"+id+"/lines")["data"].([]any)
	require.NotEmpty(t, lines[0].(Object)["id"])
	require.Equal(t, float64(now.Unix()), lines[0].(Object)["period"].(Object)["start"])
	allocations := readInvoiceFixture(t, m, "/v1/invoice_payments?invoice="+id)["data"].([]any)
	require.Len(t, allocations, 1)
	allocation := allocations[0].(Object)
	require.Equal(t, id, allocation["invoice"])
	require.Equal(t, float64(999), allocation["amount_paid"])
	require.Equal(t, "usd", allocation["currency"])
	require.Equal(t, float64(now.Unix()), allocation["status_transitions"].(Object)["paid_at"])
	intent := readInvoiceFixture(t, m, "/v1/payment_intents/"+allocation["payment"].(Object)["payment_intent"].(string))
	charge := readInvoiceFixture(t, m, "/v1/charges/"+intent["latest_charge"].(string))
	require.Equal(t, allocation["amount_paid"], intent["amount_received"])
	require.Equal(t, intent["amount_received"], charge["amount_captured"])
	require.Equal(t, true, charge["paid"])
	require.Equal(t, true, charge["captured"])
}

func TestFinalizingDraftPreservesItsBillIdentity(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	m := NewUnstarted(Options{Clock: func() time.Time { return now }})
	sub := m.AddSubscription("cus_draft", "pm_draft", "price_legacy_month", 999, now, now.Add(720*time.Hour))
	draft := m.DraftRenewal(sub)["latest_invoice"].(Object)
	require.Equal(t, "draft", draft["status"])
	require.Len(t, m.Ledger(""), 1)
	paid := m.CollectDraft(sub)
	require.Equal(t, draft["id"], paid["id"])
	require.Equal(t, draft["lines"], paid["lines"])
	require.Equal(t, "paid", paid["status"])
	require.Equal(t, float64(0), paid["amount_remaining"])
	require.Equal(t, paid, m.CollectDraft(sub), "replaying finalization cannot create a second capture")
	require.Len(t, m.Ledger(""), 2)
}

func TestZeroDueInvoiceCreatesNoCapture(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	m := NewUnstarted(Options{Clock: func() time.Time { return now }})
	sub := m.AddSubscription("cus_free", "pm_free", "price_legacy_free", 0, now, now.Add(720*time.Hour))
	invoice := m.Subscription(sub)["latest_invoice"].(Object)
	require.Equal(t, "paid", invoice["status"])
	require.Empty(t, invoice["payments"].(Object)["data"])
	require.Empty(t, m.Ledger(""))
}
