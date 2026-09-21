package intents

import (
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/providerqualification"
)

// ValidateProviderCutoverLineage explains a historical account/reference through
// retained, completed forward cutovers. Original payment custody stays bound to
// the original account; current catalog and current account defaults are unused.
func ValidateProviderCutoverLineage(merchantID, subscriptionID, customerID, sourcePSP uuid.UUID, sourceRef string, currentPSP uuid.UUID, currentRef string, operations []gen.OpenrailsRailIntent) error {
	type address struct {
		psp uuid.UUID
		ref string
	}
	next := map[address]address{}
	for _, in := range operations {
		p, g, err := decodeCutover(in)
		if err != nil {
			return err
		}
		if in.MerchantID != merchantID || in.IntentType != TypeNMIProviderCutover || in.Rail != "nmi" || in.Status != StatusSucceeded || in.SubscriptionID == nil || *in.SubscriptionID != subscriptionID || p.SubscriptionID != subscriptionID || p.CustomerID != customerID || in.PspID == nil || *in.PspID != p.Request.ExpectedTargetPSPID || p.Request.ExpectedSourcePSPID == p.Request.ExpectedTargetPSPID || p.SourceSubscriptionID == "" || g.Target == nil || g.ActivatedTarget == nil || g.SourceReceipt == nil || g.Decision == nil || g.Decision.Action != "complete" || g.Abandoned || g.NotExecuted || !g.CreateSubmitted || !g.SourceCanceled || !g.TargetActive || g.SourceAbsentAt.IsZero() {
			return errors.New("initial membership has an unqualified provider transition")
		}
		if _, _, err := cutoverAccountBindings(in, p); err != nil {
			return err
		}
		if (p.SourceQualification.Environment != "test" && p.SourceQualification.Environment != "live") || *p.SourceQualification.CredentialVersion < 0 || *p.TargetQualification.CredentialVersion < 0 || !providerqualification.ValidEvidenceReference(p.SourceQualification.EvidenceRef) || !providerqualification.ValidEvidenceReference(p.TargetQualification.EvidenceRef) || p.SourceInstrument.Validate() != nil || p.TargetInstrument.Validate() != nil || p.SourceInstrument.CustodianHeld() || p.TargetInstrument.CustodianHeld() || p.SourceInstrument.PSPID != p.Request.ExpectedSourcePSPID || p.TargetInstrument.PSPID != p.Request.ExpectedTargetPSPID {
			return errors.New("provider transition has another accepted account binding")
		}
		if g.SourceReceipt.ID != p.SourceSubscriptionID || g.SourceReceipt.CustomerVaultID != p.SourceInstrument.RailCustomerRef || !strings.EqualFold(strings.TrimSpace(g.SourceReceipt.DelayedCondition), "inactive") {
			return errors.New("provider transition has no exact source cancellation")
		}
		terms := p
		if !g.BillingAnchor.IsZero() {
			terms.Anchor = g.BillingAnchor
		}
		paused, known := cutoverPaused(g.ActivatedTarget.PausedSubscription)
		if g.Target.ID == "" || !cutoverSubscriptionMatches(*g.ActivatedTarget, terms, g.Target.ID) || !known || paused {
			return errors.New("provider transition has no exact active target")
		}
		from, to := address{p.Request.ExpectedSourcePSPID, p.SourceSubscriptionID}, address{p.Request.ExpectedTargetPSPID, g.Target.ID}
		if _, exists := next[from]; exists {
			return errors.New("provider transition lineage is ambiguous")
		}
		next[from] = to
	}
	at, want := address{sourcePSP, sourceRef}, address{currentPSP, currentRef}
	seen := map[address]bool{}
	for at != want {
		if seen[at] {
			return errors.New("provider transition lineage has a cycle")
		}
		seen[at] = true
		following, ok := next[at]
		if !ok {
			return errors.New("current membership has no completed transition from its initial account")
		}
		at = following
	}
	if len(seen) != len(next) {
		return errors.New("provider transition lineage contains disconnected history")
	}
	if _, continues := next[at]; continues {
		return errors.New("current membership precedes a completed provider transition")
	}
	return nil
}
