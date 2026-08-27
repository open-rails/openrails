package nmidirect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func TestStoredCredentialFor_ContextMapping(t *testing.T) {
	cases := []struct {
		name string
		ctx  charge.Context
		want nmi.StoredCredential
	}{
		{
			"initial one-time CIT",
			charge.InitialOneTime(),
			nmi.StoredCredential{InitiatedBy: "customer", Indicator: "stored"},
		},
		{
			"one-time reuse CIT",
			charge.OneTimeReuse("ref-1"),
			nmi.StoredCredential{InitiatedBy: "customer", Indicator: "used", InitialTransactionID: "ref-1"},
		},
		{
			"initial recurring CIT",
			charge.InitialRecurring(),
			nmi.StoredCredential{InitiatedBy: "customer", Indicator: "stored", Recurring: true},
		},
		{
			"recurring reuse CIT",
			charge.RecurringReuse("ref-2"),
			nmi.StoredCredential{InitiatedBy: "customer", Indicator: "used", InitialTransactionID: "ref-2", Recurring: true},
		},
		{
			"recurring MIT",
			charge.RecurringMIT("ref-3"),
			nmi.StoredCredential{InitiatedBy: "merchant", Indicator: "used", InitialTransactionID: "ref-3", Recurring: true},
		},
		{
			"unscheduled MIT",
			charge.UnscheduledMIT("ref-4"),
			nmi.StoredCredential{InitiatedBy: "merchant", Indicator: "used", InitialTransactionID: "ref-4"},
		},
		{
			"reference-less MIT maps to an invalid used credential",
			charge.UnscheduledMIT(""),
			nmi.StoredCredential{InitiatedBy: "merchant", Indicator: "used"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StoredCredentialFor(tc.ctx)
			require.NotNil(t, got)
			assert.Equal(t, tc.want, *got)
			if tc.ctx.Initiator == charge.InitiatorMerchant && tc.ctx.PriorRef == "" {
				require.ErrorContains(t, got.Validate(), "requires initial_transaction_id")
			}
		})
	}
}

func newChargerAgainst(t *testing.T, handler http.HandlerFunc) *Charger {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := nmi.NewClient("nmi", &config.NMIProviderSettings{SecurityKey: "k"}, true)
	require.NoError(t, err)
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL
	return New(client)
}

func baseRequest(ctx charge.Context) charge.Request {
	return charge.Request{
		Instrument:  charge.Instrument{Rail: "nmi", CustomerRef: "v1", MethodRef: "b1"},
		AmountMinor: moneyutil.Cents(500),
		Currency:    "USD",
		Description: "test charge",
		OrderRef:    "order-1",
		Context:     ctx,
	}
}

func TestCharger_InitialCITCapturesRefAndAnchoredMITReplaysIt(t *testing.T) {
	var form url.Values
	c := newChargerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form = r.Form
		fmt.Fprint(w, "response=1&transactionid=txn-99&authcode=OK&response_code=100")
	})

	// The customer-present initial CIT anchors the sequence.
	res, err := c.Charge(context.Background(), baseRequest(charge.InitialOneTime()))
	require.NoError(t, err)
	assert.Equal(t, "txn-99", res.TransactionID)
	assert.Equal(t, "txn-99", res.CapturedRef, "initial CIT anchors on its transaction id")
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "stored", form.Get("stored_credential_indicator"))

	// Anchored: nothing new to capture, the anchor rides the wire.
	res, err = c.Charge(context.Background(), baseRequest(charge.UnscheduledMIT("anchor-1")))
	require.NoError(t, err)
	assert.Empty(t, res.CapturedRef)
	assert.Equal(t, "anchor-1", form.Get("initial_transaction_id"))
}

func TestCharger_ReferenceLessMITFailsBeforeNetwork(t *testing.T) {
	requests := 0
	c := newChargerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		t.Error("reference-less MIT must not reach NMI")
	})

	_, err := c.Charge(context.Background(), baseRequest(charge.UnscheduledMIT("")))
	require.ErrorContains(t, err, "requires initial_transaction_id")
	assert.Zero(t, requests)
}

func TestCharger_HardDeclineIsResultNotError(t *testing.T) {
	c := newChargerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "response=2&responsetext=DECLINED&response_code=202")
	})
	res, err := c.Charge(context.Background(), baseRequest(charge.UnscheduledMIT("anchor-1")))
	require.NoError(t, err)
	assert.True(t, res.Declined)
	require.NotNil(t, res.FailureCode)
	assert.Equal(t, "insufficient_funds", *res.FailureCode)
}

func TestCharger_TransientGatewayConditionIsError(t *testing.T) {
	// 430 duplicate-transaction: transient, never a hard decline.
	c := newChargerAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "response=2&responsetext=Duplicate&response_code=430")
	})
	_, err := c.Charge(context.Background(), baseRequest(charge.UnscheduledMIT("anchor-1")))
	require.Error(t, err)
}
