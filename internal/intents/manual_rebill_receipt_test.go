package intents

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

func TestManualRebillReceiptBindsFrozenOperation(t *testing.T) {
	p := testManualRebillPayload()
	row := manualRebillIntent(t, p)
	psp := p.Instrument.PSPID
	row.MerchantID, row.PspID, row.SubscriptionID = uuid.New(), &psp, &p.SubscriptionID
	receipt := manualRebillReceipt{transactionID: "txn-qualified", binding: rebillReceiptBinding(row, p, "txn-qualified")}
	row.ResultEvidence, _ = json.Marshal(map[string]any{rebillReceiptKey: storedRebillReceipt{receipt.transactionID, receipt.binding}})
	loaded, found, err := loadRebillReceipt(row, p)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, receipt, loaded)

	for _, tc := range []struct {
		name   string
		mutate func(*gen.OpenrailsRailIntent, *ManualRebillPayload)
	}{
		{"operation", func(r *gen.OpenrailsRailIntent, _ *ManualRebillPayload) { r.ID = uuid.New() }},
		{"operation kind", func(r *gen.OpenrailsRailIntent, _ *ManualRebillPayload) { r.IntentType = "other" }},
		{"merchant", func(r *gen.OpenrailsRailIntent, _ *ManualRebillPayload) { r.MerchantID = uuid.New() }},
		{"provider", func(r *gen.OpenrailsRailIntent, _ *ManualRebillPayload) { v := uuid.New(); r.PspID = &v }},
		{"subject", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.SubscriptionID = uuid.New() }},
		{"custody", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.Instrument.Custodian = "basistheory" }},
		{"frozen provider", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.Instrument.PSPID = uuid.New() }},
		{"vault", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.Instrument.RailCustomerRef = "other" }},
		{"amount", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.AmountMinor++ }},
		{"currency", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.Currency = "EUR" }},
		{"billing reference", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.Instrument.RailMethodRef = "other" }},
		{"subscription reference", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.RailSubscriptionID = "other" }},
		{"credential anchor", func(_ *gen.OpenrailsRailIntent, p *ManualRebillPayload) { p.CredentialReference = "other" }},
		{"transaction", func(r *gen.OpenrailsRailIntent, _ *ManualRebillPayload) {
			r.ResultEvidence, _ = json.Marshal(map[string]any{rebillReceiptKey: storedRebillReceipt{"other", receipt.binding}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, terms := row, p
			tc.mutate(&changed, &terms)
			_, found, err := loadRebillReceipt(changed, terms)
			require.True(t, found)
			require.Error(t, err)
		})
	}
	row.ResultEvidence = []byte(`{"transaction_id":"candidate-only"}`)
	_, found, err = loadRebillReceipt(row, p)
	require.NoError(t, err)
	require.False(t, found, "candidate IDs cannot construct a qualified receipt")
	require.Error(t, (&ManualRebillHandler{}).finalizeSuccess(nil, row, p, manualRebillReceipt{}), "an empty receipt cannot enter local finalization")
}
