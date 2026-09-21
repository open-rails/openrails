package nmi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestRecurringSaleEvidenceExactAccountOrderTransactionAndSoleBilling(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, vault, billing, returnedID, currency, amount string
		duplicate, absent                                             bool
		invalid                                                       bool
	}{
		{name: "qualified", candidate: "txn", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00"},
		{name: "recovered without response", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00"},
		{name: "wrong candidate", candidate: "other", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00", invalid: true},
		{name: "wrong vault", candidate: "txn", vault: "other", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00", invalid: true},
		{name: "changed billing", candidate: "txn", vault: "v1", billing: "b2", returnedID: "txn", currency: "USD", amount: "5.00", invalid: true},
		{name: "wrong exact transaction", candidate: "txn", vault: "v1", billing: "b1", returnedID: "other", currency: "USD", amount: "5.00", invalid: true},
		{name: "missing currency", candidate: "txn", vault: "v1", billing: "b1", returnedID: "txn", amount: "5.00", invalid: true},
		{name: "inexact amount", candidate: "txn", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.001", invalid: true},
		{name: "duplicate successful order", candidate: "txn", vault: "v1", billing: "b1", duplicate: true, invalid: true},
		{name: "delayed visibility", candidate: "txn", absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				switch r.URL.Path {
				case "/query":
					require.NoError(t, r.ParseForm())
					require.Equal(t, "pinned-key", r.Form.Get("security_key"))
					require.Equal(t, "accepted-order", r.Form.Get("order_id"))
					fmt.Fprint(w, "<nm_response>")
					if !tc.absent {
						fmt.Fprint(w, `<transaction><transaction_id>txn</transaction_id><order_id>accepted-order</order_id><action><action_type>sale</action_type><success>1</success></action></transaction>`)
					}
					if tc.duplicate {
						fmt.Fprint(w, `<transaction><transaction_id>other</transaction_id><order_id>accepted-order</order_id><action><action_type>sale</action_type><success>1</success></action></transaction>`)
					}
					fmt.Fprint(w, "</nm_response>")
				case "/payments/txn":
					require.Equal(t, http.MethodGet, r.Method)
					require.Equal(t, "pinned-key", r.Header.Get("Authorization"))
					require.NoError(t, json.NewEncoder(w).Encode(v5Transaction{Object: "transaction", ID: tc.returnedID, CustomerVaultID: tc.vault, Response: "1", Currency: tc.currency, Amount: tc.amount, Actions: []v5TxnAction{{Type: "sale", Amount: tc.amount, Success: true}}}))
				case "/customers/v1":
					require.Equal(t, http.MethodGet, r.Method)
					require.Equal(t, "pinned-key", r.Header.Get("Authorization"))
					fmt.Fprintf(w, `{"object":"customer","id":"v1","billing":[{"id":%q}]}`, tc.billing)
				default:
					t.Errorf("receipt attempted unexpected provider request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			c, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "pinned-key"}, true)
			require.NoError(t, err)
			c.QueryURL = server.URL + "/query"
			c.V5BaseURL = server.URL
			c.DirectPostURL = server.URL + "/forbidden-sale"
			// Recovery remains possible in readonly mode and is pinned to the captured
			// account credentials even if the exported legacy field is modified.
			c.SecurityKey = "wrong-key"
			c.ReadOnly = true
			facts, found, err := c.ReadRecurringSaleEvidence(t.Context(), "accepted-order", tc.candidate, "v1", "b1")
			if tc.invalid {
				require.Error(t, err)
				require.False(t, found)
				return
			}
			require.NoError(t, err)
			if tc.absent {
				require.False(t, found)
				require.Equal(t, 1, reads)
				return
			}
			require.True(t, found)
			require.Equal(t, 3, reads)
			require.Equal(t, "txn", facts.TransactionID)
			require.Equal(t, "v1", facts.CustomerVaultID)
			require.Equal(t, "b1", facts.VaultBillingID)
			require.Equal(t, "USD", facts.Currency)
			require.EqualValues(t, 500, facts.Amount)
		})
	}
}

func TestRecurringSaleEvidenceRejectsMissingIdentityBeforeRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unqualified recurring receipt reached provider")
	}))
	defer server.Close()
	c := newTestClient(t, server.URL)
	_, _, err := c.ReadRecurringSaleEvidence(t.Context(), "order", "txn", "v1", "b1")
	require.Error(t, err)
	scoped, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "key"}, true)
	require.NoError(t, err)
	scoped.QueryURL = server.URL
	scoped.V5BaseURL = server.URL
	_, _, err = scoped.ReadRecurringSaleEvidence(t.Context(), "order", "txn", "v1", "")
	require.Error(t, err)
}
