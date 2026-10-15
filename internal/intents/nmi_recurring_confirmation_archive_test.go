package intents

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

func TestRecurringConfirmationArchiveRequiresBoundTerminalEvidence(t *testing.T) {
	merchant, customer, sub, method, psp := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	terms := confirmationTerms{SubscriptionID: sub, CustomerID: customer, PaymentMethodID: method, Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault-1"}, Rail: "nmi", Schedule: "schedule-1", PeriodEnd: at}
	raw, err := json.Marshal(terms)
	require.NoError(t, err)
	episode := uuid.NewSHA1(merchant, raw).String()
	payload, err := json.Marshal(recurringConfirmation{terms, "pmr-" + episode, at})
	require.NoError(t, err)
	actor := customer.String()
	accepted := gen.BillingProviderIntent{ID: uuid.New(), MerchantID: merchant, PspID: &psp, SubscriptionID: &sub, Rail: "nmi", IntentType: TypeNMIRecurringConfirmation, Origin: string(OriginUser), Actor: &actor, Payload: payload, IdempotencyKey: TypeNMIRecurringConfirmation + ":" + episode, Status: StatusSucceeded}
	const approval = `{"confirmation_submitted_at":"2026-10-15T12:00:00Z","confirmation_answer":{"transaction_id":"verify-1","response_code":100},"transaction_id":"verify-1","response_code":100}`
	for _, tc := range []struct {
		name, status, evidence string
		change                 func(*gen.BillingProviderIntent)
		valid                  bool
	}{
		{name: "approved", status: StatusSucceeded, evidence: approval, valid: true},
		{name: "approved but subscription changed", status: StatusFailedTerminal, evidence: approval, valid: true},
		{name: "declined", status: StatusFailedTerminal, evidence: `{"confirmation_submitted_at":"2026-10-15T12:00:00Z","confirmation_answer":{"transaction_id":"declined-1","response_code":200},"transaction_id":"declined-1","response_code":200}`, valid: true},
		{name: "not submitted", status: StatusFailedTerminal, evidence: `{"not_executed":true}`, valid: true},
		{name: "unknown remains unportable", status: StatusUnknownNeedsVerify, evidence: approval},
		{name: "success without answer", status: StatusSucceeded, evidence: `{}`},
		{name: "contradictory projection", status: StatusSucceeded, evidence: `{"confirmation_submitted_at":"2026-10-15T12:00:00Z","confirmation_answer":{"transaction_id":"verify-1","response_code":100},"transaction_id":"different","response_code":100}`},
		{name: "false nonexecution", status: StatusFailedTerminal, evidence: `{"not_executed":true,"confirmation_submitted_at":"2026-10-15T12:00:00Z"}`},
		{name: "other account", status: StatusSucceeded, evidence: approval, change: func(in *gen.BillingProviderIntent) { id := uuid.New(); in.PspID = &id }},
		{name: "other customer authority", status: StatusSucceeded, evidence: approval, change: func(in *gen.BillingProviderIntent) { id := uuid.NewString(); in.Actor = &id }},
		{name: "different episode", status: StatusSucceeded, evidence: approval, change: func(in *gen.BillingProviderIntent) {
			in.IdempotencyKey = TypeNMIRecurringConfirmation + ":" + uuid.NewString()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := accepted
			in.Status = tc.status
			in.ResultEvidence = []byte(tc.evidence)
			if tc.change != nil {
				tc.change(&in)
			}
			err := ValidateNMIRecurringConfirmationTerminal(in)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
