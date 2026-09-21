//go:build integration

package intents

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"testing"
)

// TerminalRebillArchiveFixture gives the external archive test the same real
// admission/HTTP/receipt/lifecycle workflow without importing archive into the
// intents package (the contract depends on the canonical intent decoder).
func TerminalRebillArchiveFixture(t *testing.T, refused bool) (*db.DB, merchant.ID, gen.OpenrailsRailIntent, func() int64) {
	return terminalRebillArchiveFixture(t, refused, false)
}

func CustomerTerminalRebillArchiveFixture(t *testing.T, refused bool) (*db.DB, merchant.ID, gen.OpenrailsRailIntent, func() int64) {
	return terminalRebillArchiveFixture(t, refused, true)
}

func terminalRebillArchiveFixture(t *testing.T, refused, customer bool) (*db.DB, merchant.ID, gen.OpenrailsRailIntent, func() int64) {
	t.Helper()
	fx := seedPastDueSubscriptionForMerchant(t, uuid.New())
	gateway, client := newFakeNMIRebillGateway(t, fx)
	if refused {
		gateway.saleBody.Store("response=2&response_code=202&responsetext=Insufficient+funds")
	}
	handler := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	var accepted gen.OpenrailsRailIntent
	var err error
	if customer {
		// This real key hashes to a PAN-looking digit run. Its typed identity
		// must survive export without exempting arbitrary customer text.
		accepted, _, err = handler.EnqueueCustomer(fx.handlerCtx(), fx.subID, fx.payload.Renewal.CustomerID, "archive-key-1461", nil)
	} else {
		accepted, err = handler.EnqueueScheduled(fx.handlerCtx(), fx.subID)
	}
	require.NoError(t, err)
	result, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(fx.handlerCtx(), accepted.ID)
	require.NoError(t, err)
	require.NoError(t, ValidateManualRebillTerminal(result))
	return fx.db, merchant.ID(fx.merchantID), result, gateway.saleCalls.Load
}
