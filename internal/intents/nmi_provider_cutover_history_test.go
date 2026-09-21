package intents

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/stretchr/testify/require"
)

func TestInitialMembershipProviderCutoverLineage(t *testing.T) {
	mid, sub, customer, a, b, c := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	anchor := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	makeRow := func(from, to uuid.UUID, oldRef, newRef string) gen.OpenrailsRailIntent {
		version := 1
		fingerprint := strings.Repeat("a", 64)
		qualification := func(psp uuid.UUID) providerqualification.BoundRecord {
			return providerqualification.BoundRecord{Record: providerqualification.Record{PSPID: psp, Environment: "test", Contract: providerqualification.NMIContract, EvidenceRef: "fixture-qualified"}, CredentialVersion: &version, CredentialFingerprint: fingerprint}
		}
		instrument := func(psp uuid.UUID) charge.FrozenInstrument {
			return charge.FrozenInstrument{PSPID: psp, Custodian: "psp", RailCustomerRef: "vault", RailMethodRef: "billing"}
		}
		p := nmiCutoverPayload{CustomerID: customer, SubscriptionID: sub, SourceSubscriptionID: oldRef, SourceInstrument: instrument(from), TargetInstrument: instrument(to), Request: nmiCutoverRequest{ExpectedSourcePSPID: from, ExpectedTargetPSPID: to, TargetPaymentMethodID: uuid.New()}, SourceQualification: qualification(from), TargetQualification: qualification(to), SourceCredentialFingerprint: fingerprint, TargetCredentialFingerprint: fingerprint, Amount: 1000000, Currency: "usd", CycleHours: 24, PlanID: "plan", Anchor: anchor}
		target := nmi.V5Subscription{Object: "subscription", ID: newRef, CustomerVaultID: "vault", DelayedCondition: "active", PausedSubscription: false, Amount: "1.00", NextBillingDate: anchor.Format(time.RFC3339), Plan: &nmi.V5Plan{ID: "plan", PlanAmount: "1.00", PlanPayments: "0", DayFrequency: "1"}}
		g := nmiCutoverProgress{Decision: &nmiCutoverDecision{Action: "complete"}, CreateSubmitted: true, SourceCanceled: true, SourceAbsentAt: anchor.Add(-time.Hour), SourceReceipt: &nmi.V5Subscription{Object: "subscription", ID: oldRef, CustomerVaultID: "vault", DelayedCondition: "inactive"}, TargetActive: true, Target: &target, ActivatedTarget: &target}
		payload, err := json.Marshal(p)
		require.NoError(t, err)
		evidence, err := json.Marshal(g)
		require.NoError(t, err)
		return gen.OpenrailsRailIntent{ID: uuid.New(), MerchantID: mid, Rail: "nmi", IntentType: TypeNMIProviderCutover, Status: StatusSucceeded, SubscriptionID: &sub, PspID: &to, Payload: payload, ResultEvidence: evidence}
	}
	ab, bc := makeRow(a, b, "A", "B"), makeRow(b, c, "B", "C")
	check := func(rows []gen.OpenrailsRailIntent) error {
		return ValidateProviderCutoverLineage(mid, sub, customer, a, "A", c, "C", rows)
	}
	require.NoError(t, check([]gen.OpenrailsRailIntent{bc, ab}), "identity links are independent of archive row order")
	for _, tc := range []struct {
		name string
		rows []gen.OpenrailsRailIntent
	}{{"missing", []gen.OpenrailsRailIntent{bc}}, {"wrong source", []gen.OpenrailsRailIntent{makeRow(a, b, "another", "B"), bc}}, {"ambiguous", []gen.OpenrailsRailIntent{ab, ab, bc}}, {"cycle", []gen.OpenrailsRailIntent{ab, makeRow(b, a, "B", "A")}}} {
		t.Run(tc.name, func(t *testing.T) { require.Error(t, check(tc.rows)) })
	}
	for _, field := range []string{"abandon", "customer", "source receipt", "target receipt", "qualification"} {
		t.Run(field, func(t *testing.T) {
			row := ab
			p, g, err := decodeCutover(row)
			require.NoError(t, err)
			switch field {
			case "abandon":
				g.Decision.Action = "abandon"
			case "customer":
				p.CustomerID = uuid.New()
			case "source receipt":
				g.SourceReceipt.DelayedCondition = "active"
			case "target receipt":
				g.ActivatedTarget.CustomerVaultID = "wrong"
			case "qualification":
				p.TargetQualification.PSPID = uuid.New()
			}
			row.Payload, err = json.Marshal(p)
			require.NoError(t, err)
			row.ResultEvidence, err = json.Marshal(g)
			require.NoError(t, err)
			require.Error(t, check([]gen.OpenrailsRailIntent{row, bc}))
		})
	}
}
