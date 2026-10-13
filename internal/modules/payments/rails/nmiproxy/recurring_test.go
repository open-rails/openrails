package nmiproxy

import (
	"net/http"
	"testing"

	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

func TestRecurringCharge(t *testing.T) {
	for _, scenario := range []struct {
		name, response      string
		declined, uncertain bool
	}{
		{"paid", "response=1&transactionid=1002&response_code=100", false, false},
		{"declined", "response=2&transactionid=1002&response_code=202", true, false},
		{"duplicate", "response=3&response_code=430", false, true},
		{"unreadable", "response=2&response=1", false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			proxy := &fakeProxy{serve: func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "merchant", r.PostForm.Get("initiated_by"))
				require.Equal(t, "used", r.PostForm.Get("stored_credential_indicator"))
				require.Equal(t, "recurring", r.PostForm.Get("billing_method"))
				require.Equal(t, "1001", r.PostForm.Get("initial_transaction_id"))
				require.Equal(t, "1.99", r.PostForm.Get("amount"))
				require.Empty(t, r.PostForm.Get("cvv"))
				gatewayReply(scenario.response)(w, r)
			}}
			charger := newProxyCharger(t, proxy).WithSource(Source{TokenID: tokenID})
			req := charge.Request{Instrument: charge.Instrument{Rail: Rail, MethodRef: tokenID}, AmountMinor: 199, Currency: "USD", OrderRef: "period-1",
				Context: charge.Merchant(charge.AgreementRecurring, cites(charge.AgreementRecurring, "1001"))}
			res, refusal, err := charger.ChargeRecurringMIT(t.Context(), req)
			if scenario.uncertain {
				require.Error(t, err)
				require.Nil(t, refusal)
			} else {
				require.NoError(t, err)
				if scenario.declined {
					require.NotNil(t, refusal)
					require.Equal(t, 202, refusal.ResponseCode)
				} else {
					require.Nil(t, refusal)
					require.Equal(t, "1002", res.TransactionID)
				}
			}
			require.Equal(t, 1, proxy.calls)
			req.Context = charge.Merchant(charge.AgreementCardOnFile, cites(charge.AgreementCardOnFile, "1001"))
			_, _, err = charger.ChargeRecurringMIT(t.Context(), req)
			require.ErrorIs(t, err, charge.ErrNotDispatched)
			require.Equal(t, 1, proxy.calls, "reuse consent cannot authorize a renewal")
		})
	}
}

func TestVerifyRecurring(t *testing.T) {
	proxy := &fakeProxy{serve: func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		require.Equal(t, "validate", r.PostForm.Get("type"))
		require.Equal(t, "0.00", r.PostForm.Get("amount"))
		require.Equal(t, "customer", r.PostForm.Get("initiated_by"))
		require.Equal(t, "stored", r.PostForm.Get("stored_credential_indicator"))
		require.Equal(t, "recurring", r.PostForm.Get("billing_method"))
		require.Empty(t, r.PostForm.Get("initial_transaction_id"))
		require.Empty(t, r.PostForm.Get("cvv"))
		gatewayReply("response=1&transactionid=1003&response_code=100")(w, r)
	}}
	charger := newProxyCharger(t, proxy).WithSource(Source{TokenID: tokenID})
	ref, err := charger.VerifyRecurring(t.Context(), "replace-1")
	require.NoError(t, err)
	require.Equal(t, "1003", ref)
	require.Equal(t, 1, proxy.calls)
}
