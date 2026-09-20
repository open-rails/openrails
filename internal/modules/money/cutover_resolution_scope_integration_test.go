//go:build integration

package money_test

import (
	"testing"

	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func TestCutoverAbandonCannotResolveInvoiceCollection(t *testing.T) {
	e := nmiReceiptScenario(t)
	before := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	_, err := e.runner.Resolve(e.ctx, before.ID, intents.Resolution{
		Step: "target", Abandon: true, Actor: "operator", Reason: "cutover-only action",
	})
	require.ErrorIs(t, err, intents.ErrResolutionUnsupported)
	_, err = e.runner.Resolve(e.ctx, before.ID, intents.Resolution{Step: "source", RequalifyAccount: "account-proof", Actor: "operator", Reason: "cutover-only requalification"})
	require.ErrorIs(t, err, intents.ErrResolutionUnsupported)
	after := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.ClaimedUntil, after.ClaimedUntil)
	require.Equal(t, before.Payload, after.Payload)
	require.Equal(t, before.ResultEvidence, after.ResultEvidence)
}
