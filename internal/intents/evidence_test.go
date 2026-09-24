package intents

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A receipt binds the exact accepted integer liability; a float digest would
// collapse adjacent values above 2^53.
func TestCollectionReceiptBindsExactAcceptedAmount(t *testing.T) {
	psp := uuid.New()
	payload := InvoiceCollectionPayload{Initiator: charge.InitiatorMerchant, InvoiceID: uuid.New(), CustomerID: uuid.New(), AttemptID: uuid.New(), PaymentMethodID: uuid.New(),
		Rail: "nmi", Currency: "USD", Amount: 9_007_199_254_740_992, Instrument: charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault"}}
	minor, err := moneyutil.NativeToRailMinor(payload.Currency, payload.Amount)
	require.NoError(t, err)
	payload.AmountMinor = minor
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	in := gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: uuid.New(), PspID: &psp, IntentType: "invoice_collection", Rail: "nmi", Payload: raw}
	binding, err := collectionBinding(in)
	require.NoError(t, err)
	receipt := CollectedReceipt{data: collectedReceipt{Version: 1, Family: "collected_payment", Binding: binding,
		NMI: &nmi.SaleEvidence{TransactionID: "paid", OrderReference: in.ID.String(), CustomerVaultID: "vault", Amount: minor, Currency: "USD", Approved: true}}}
	require.NoError(t, receipt.Validate(in))

	payload.Amount++ // same rail charge, different accepted liability
	changed := in
	changed.Payload, err = json.Marshal(payload)
	require.NoError(t, err)
	require.Error(t, receipt.Validate(changed))
}

// Ambiguous or malformed provider proof is refused before custody storage, without echoing raw provider text.
func TestInitialMembershipDeclineRejectsAmbiguousRawProof(t *testing.T) {
	for _, raw := range []string{
		"response=2&response=1&response_code=202&response_code=100",
		"response=2&response=2&response_code=202",
		"response=2&response_code=202&response_code=202",
		"response=2&response_code=100",
		"response=2&response_code=202&transactionid=one&transactionid=two",
		"response=2&response_code=202&responsetext=%QRAW_PROVIDER_SENTINEL",
	} {
		var store *Store // nil: any storage attempt panics
		err := store.RetainInitialMembershipDecline(t.Context(), gen.OpenrailsRailIntent{}, &nmi.CustomerVaultError{ResponseCode: 202, RawResponse: raw})
		require.Error(t, err, raw)
		require.NotContains(t, err.Error(), "RAW_PROVIDER_SENTINEL")
	}
}

func TestCutoverProviderShapes(t *testing.T) {
	for _, tc := range []struct {
		micros   int64
		provider string
		matches  bool
	}{
		{10_000, "0.01", true},
		{19_990_000, "19.99", true},
		{9_007_199_254_750_000, "9007199254.75", true},
		{19_990_000, "1999.00", false}, // hundredfold drift
		{19_990_001, "19.99", false},   // sub-cent is not rounded away
	} {
		p := nmiCutoverPayload{Currency: "usd", Amount: tc.micros, CycleHours: 720, PlanID: "exact-plan"}
		plan := nmi.V5Plan{ID: p.PlanID, PlanAmount: tc.provider, PlanPayments: "0", DayFrequency: "30"}
		require.Equal(t, tc.matches, cutoverPlanMatches(plan, p), "%d vs %s", tc.micros, tc.provider)
	}
	for raw, want := range map[string][2]bool{
		`true`: {true, true}, `false`: {false, true}, `1`: {true, true}, `0`: {false, true}, `"1"`: {true, true}, `"0"`: {false, true},
		`null`: {false, false}, `2`: {false, false}, `0.5`: {false, false}, `{}`: {false, false}, `"true"`: {false, false},
	} {
		var value any
		require.NoError(t, json.Unmarshal([]byte(raw), &value))
		paused, known := cutoverPaused(value)
		require.Equal(t, want, [2]bool{paused, known}, raw)
	}
}

func TestProviderCutoverLineage(t *testing.T) {
	mid, sub, customer, a, b, c := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	anchor := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	version, fingerprint := 1, strings.Repeat("a", 64)
	qualification := func(psp uuid.UUID) providerqualification.BoundRecord {
		return providerqualification.BoundRecord{Record: providerqualification.Record{PSPID: psp, Environment: "test", Contract: providerqualification.NMIContract, EvidenceRef: "fixture-qualified"},
			CredentialVersion: &version, CredentialFingerprint: fingerprint}
	}
	instrument := func(psp uuid.UUID) charge.FrozenInstrument {
		return charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault", RailMethodRef: "billing"}
	}
	encode := func(row gen.OpenrailsRailIntent, p nmiCutoverPayload, g nmiCutoverProgress) gen.OpenrailsRailIntent {
		var err error
		row.Payload, err = json.Marshal(p)
		require.NoError(t, err)
		row.ResultEvidence, err = json.Marshal(g)
		require.NoError(t, err)
		return row
	}
	hop := func(from, to uuid.UUID, oldRef, newRef string) gen.OpenrailsRailIntent {
		p := nmiCutoverPayload{CustomerID: customer, SubscriptionID: sub, SourceSubscriptionID: oldRef, SourceInstrument: instrument(from), TargetInstrument: instrument(to),
			Request:             nmiCutoverRequest{ExpectedSourcePSPID: from, ExpectedTargetPSPID: to, TargetPaymentMethodID: uuid.New()},
			SourceQualification: qualification(from), TargetQualification: qualification(to), SourceCredentialFingerprint: fingerprint, TargetCredentialFingerprint: fingerprint,
			Amount: 1000000, Currency: "usd", CycleHours: 24, PlanID: "plan", Anchor: anchor}
		target := nmi.V5Subscription{Object: "subscription", ID: newRef, CustomerVaultID: "vault", DelayedCondition: "active", PausedSubscription: false, Amount: "1.00", NextBillingDate: anchor.Format(time.RFC3339),
			Plan: &nmi.V5Plan{ID: "plan", PlanAmount: "1.00", PlanPayments: "0", DayFrequency: "1"}}
		g := nmiCutoverProgress{Decision: &nmiCutoverDecision{Action: "complete"}, CreateSubmitted: true, SourceCanceled: true, SourceAbsentAt: anchor.Add(-time.Hour),
			SourceReceipt: &nmi.V5Subscription{Object: "subscription", ID: oldRef, CustomerVaultID: "vault", DelayedCondition: "inactive"}, TargetActive: true, Target: &target, ActivatedTarget: &target}
		return encode(gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: mid, Rail: "nmi", IntentType: TypeNMIProviderCutover, Status: StatusSucceeded, SubscriptionID: &sub, PspID: &to}, p, g)
	}
	check := func(rows ...gen.OpenrailsRailIntent) error {
		return ValidateProviderCutoverLineage(mid, sub, customer, a, "A", c, "C", rows)
	}
	ab, bc := hop(a, b, "A", "B"), hop(b, c, "B", "C")
	require.NoError(t, check(bc, ab), "links are independent of archive row order")
	require.Error(t, check(bc), "missing hop")
	require.Error(t, check(hop(a, b, "another", "B"), bc), "wrong source")
	require.Error(t, check(ab, ab, bc), "ambiguous hop")
	require.Error(t, check(ab, hop(b, a, "B", "A")), "cycle")

	for field, mutate := range map[string]func(*nmiCutoverPayload, *nmiCutoverProgress){
		"abandoned":      func(_ *nmiCutoverPayload, g *nmiCutoverProgress) { g.Decision.Action = "abandon" },
		"customer":       func(p *nmiCutoverPayload, _ *nmiCutoverProgress) { p.CustomerID = uuid.New() },
		"source receipt": func(_ *nmiCutoverPayload, g *nmiCutoverProgress) { g.SourceReceipt.DelayedCondition = "active" },
		"target receipt": func(_ *nmiCutoverPayload, g *nmiCutoverProgress) { g.ActivatedTarget.CustomerVaultID = "wrong" },
		"qualification":  func(p *nmiCutoverPayload, _ *nmiCutoverProgress) { p.TargetQualification.PSPID = uuid.New() },
	} {
		p, g, err := decodeCutover(ab)
		require.NoError(t, err)
		mutate(&p, &g)
		require.Error(t, check(encode(ab, p, g), bc), field)
	}
}
