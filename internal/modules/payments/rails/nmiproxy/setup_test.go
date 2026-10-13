package nmiproxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyCard(t *testing.T) {
	for _, response := range []string{"response=1&transactionid=1004&response_code=100", "response=2&response_code=200", "response=1&response_code=100", "unreadable"} {
		t.Run(response, func(t *testing.T) {
			proxy := &fakeProxy{serve: func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				require.Equal(t, "validate", r.PostForm.Get("type"))
				require.Equal(t, "0.00", r.PostForm.Get("amount"))
				require.Equal(t, "customer", r.PostForm.Get("initiated_by"))
				require.Equal(t, "stored", r.PostForm.Get("stored_credential_indicator"))
				require.Empty(t, r.PostForm.Get("billing_method"))
				require.Empty(t, r.PostForm.Get("initial_transaction_id"))
				require.Contains(t, r.PostForm.Get("cvv"), "token_intent: "+intentID)
				gatewayReply(response)(w, r)
			}}
			charger := newProxyCharger(t, proxy).WithSource(Source{TokenIntentID: intentID})
			ref, err := charger.VerifyCard(t.Context(), "setup-1")
			if response == "response=1&transactionid=1004&response_code=100" {
				require.NoError(t, err)
				require.Equal(t, "1004", ref)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, 1, proxy.calls)
		})
	}
}
