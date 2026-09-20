package nmi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

func TestRefundQualificationRequiresExactCurrencyAndAmount(t *testing.T) {
	for _, tc := range []struct {
		name, currency, amount string
		minor                  moneyutil.Cents
		invalid                bool
	}{
		{"dollars", "USD", "1.00", 100, false},
		{"yen", "JPY", "100.00", 100, false},
		{"fractional yen", "JPY", "100.01", 100, true},
		{"unparseable refund", "USD", "unknown", 100, true},
		{"missing currency", "", "1.00", 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				id := strings.TrimPrefix(r.URL.Path, "/payments/")
				require.Contains(t, []string{"original", "refund"}, id)
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(v5Transaction{
					ID: id, Amount: tc.amount, Currency: tc.currency, Response: "1", CustomerVaultID: "vault",
					Actions: []v5TxnAction{{ID: "refund", Type: "refund", Amount: tc.amount, Success: true}},
				}))
			}))
			t.Cleanup(server.Close)
			err := newTestClient(t, server.URL).ConfirmRefund(t.Context(), "original", "refund", tc.minor, tc.currency)
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRefundRetainsCurrencyAcrossTheMinorUnitBoundary(t *testing.T) {
	// The admin refund boundary uses this same currency-aware conversion. The
	// NMI adapter must retain that currency instead of interpreting every unit as USD cents.
	minor, err := moneyutil.NativeToRailMinorExact("JPY", 1_000_000) // 100 JPY
	require.NoError(t, err)
	require.EqualValues(t, 100, minor)
	var request map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"refund","response":"1"}`))
	}))
	t.Cleanup(server.Close)
	_, err = newTestClient(t, server.URL).Refund(t.Context(), RefundParams{TransactionID: "original", Amount: minor, Currency: "JPY"})
	require.NoError(t, err)
	require.Equal(t, "100.00", string(request["amount"]), "100 JPY must not become a 1 JPY provider refund")
}
