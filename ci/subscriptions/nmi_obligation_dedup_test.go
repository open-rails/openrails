//go:build e2e && integration

package subscriptions_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A copied book can lack even its first local operation while the provider
// already holds the renewal. The persistent order reference outlives any
// gateway duplicate window and must be read before sending attempt zero.
func TestEngineNMIFirstAttemptFindsRemoteObligation(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.refreshBeforePeriodEnd()
	initial := w.nmi.LastSale()
	paid := w.nmi.AddSale(nmimock.Sale{
		OrderID: subscriptions.ObligationOrderReference(e.sub.UUID(), end),
		Vault:   initial.Vault, BillingID: initial.BillingID, Amount: initial.Amount, At: end,
	})
	before := e.providerAttempts()
	e.toPeriodEnd()
	w.runRenewals()
	require.Equal(t, before, e.providerAttempts(), "lookup must precede the first sale submission")
	w.until(func() bool { return e.periodEnd().After(end) }, "the remote charge pays the first local renewal attempt")
	w.runRenewals()
	require.Equal(t, before, e.providerAttempts(), "recover the provider charge without sending another sale")
	require.Len(t, e.providerLedger(), 2, "one initial charge and one renewal")
	payments := completed(w.payments(embedded, e.c.id))
	require.Len(t, payments, 2)
	e.requireLedgerAgreement(payments)
	var transaction string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT result_evidence->>'transaction_id' FROM billing.provider_intents WHERE intent_type = 'subscription_collection' AND subscription_id = $1`), e.sub.UUID()).Scan(&transaction))
	require.Equal(t, paid.TransactionID, transaction)
}

func TestEngineNMIFirstAttemptWaitsForOrderLookup(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.refreshBeforePeriodEnd()
	before := e.providerAttempts()
	w.nmi.QueryUnavailable(true)
	e.toPeriodEnd()
	w.runRenewals()
	w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "an unavailable order lookup holds the renewal")
	require.Equal(t, before, e.providerAttempts(), "a failed lookup cannot authorize a charge")
	require.True(t, e.periodEnd().Equal(end))

	w.nmi.QueryUnavailable(false)
	w.until(func() bool { return e.periodEnd().After(end) }, "the renewal resumes when the order can be read")
	require.Equal(t, before+1, e.providerAttempts())
	require.Empty(t, w.openFindings("life.submission.unresolved"))
}
