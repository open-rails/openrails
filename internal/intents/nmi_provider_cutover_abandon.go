package intents

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// A direction is written once, before either account's cancellation can be
// submitted. It also fences a stale executor whose lease expired during a read.
func (h *NMIProviderCutover) claimCutoverDecision(ctx context.Context, id uuid.UUID, decision nmiCutoverDecision) (*nmiCutoverDecision, error) {
	store := NewStore(h.DB)
	_, err := store.RecordProgressIfAbsent(ctx, id, "decision", decision)
	if err != nil {
		return nil, err
	}
	row, err := store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	_, progress, err := decodeCutover(row)
	if err != nil {
		return nil, err
	}
	if progress.Decision == nil {
		return nil, errors.New("cutover direction unavailable")
	}
	return progress.Decision, nil
}

func (h *NMIProviderCutover) abandonClients(ctx context.Context, in gen.OpenrailsRailIntent, p nmiCutoverPayload) (*nmi.NMIClient, *nmi.NMIClient, error) {
	bindings, _, err := cutoverAccountBindings(in, p)
	if err != nil {
		return nil, nil, err
	}
	source, ok, err := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedSourcePSPID)
	if err != nil || !ok || source == nil || !h.boundCutoverClient(ctx, source, in.MerchantID, bindings["source"]) {
		return nil, nil, ErrResolutionRejected
	}
	target, ok, err := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &p.Request.ExpectedTargetPSPID)
	if err != nil || !ok || target == nil || !h.boundCutoverClient(ctx, target, in.MerchantID, bindings["target"]) {
		return nil, nil, ErrResolutionRejected
	}
	return source, target, nil
}

func (h *NMIProviderCutover) sourceRemainsActive(ctx context.Context, source *nmi.NMIClient, p nmiCutoverPayload) bool {
	old, active, err := source.GetCutoverSubscription(ctx, p.SourceSubscriptionID)
	paused, known := cutoverPaused(old.PausedSubscription)
	if err != nil || !active || !known || paused || old.ID != p.SourceSubscriptionID || old.CustomerVaultID != p.SourceInstrument.RailCustomerRef {
		return false
	}
	// Commercial drift is allowed here: compensation never writes the source.
	local, err := h.DB.Gen(ctx).GetSubscriptionByID(ctx, p.SubscriptionID)
	return err == nil && local.MerchantID == pMerchant(ctx) && local.CustomerID == p.CustomerID &&
		local.PspID == p.Request.ExpectedSourcePSPID && local.PaymentMethodID != nil &&
		*local.PaymentMethodID == p.SourcePaymentMethodID && local.RailSubscriptionID == p.SourceSubscriptionID
}

func (h *NMIProviderCutover) requestAbandon(ctx context.Context, in gen.OpenrailsRailIntent, p nmiCutoverPayload, g nmiCutoverProgress, r Resolution) (Outcome, error) {
	if r.Step != "target" || !g.CreateSubmitted || g.Target == nil || g.SourceCancelSubmitted || g.SourceCanceled || g.ActivationSubmitted || g.TargetActive || (g.Decision != nil && g.Decision.Action != "abandon") {
		return Outcome{}, ErrResolutionRejected
	}
	source, target, err := h.abandonClients(ctx, in, p)
	if err != nil || !h.sourceRemainsActive(ctx, source, p) {
		return Outcome{}, ErrResolutionRejected
	}
	observed, active, err := target.GetCutoverSubscription(ctx, g.Target.ID)
	paused, known := cutoverPaused(observed.PausedSubscription)
	if err != nil || !active || !known || !paused || !cutoverSubscriptionMatches(observed, p, g.Target.ID) {
		return Outcome{}, ErrResolutionRejected
	}
	decision, err := h.claimCutoverDecision(ctx, in.ID, nmiCutoverDecision{Action: "abandon", Authorization: r.Record(h.now())})
	if err != nil || decision.Action != "abandon" {
		return Outcome{}, ErrResolutionRejected
	}
	return Retryable("operator authorized cancellation of the exact paused target; continue under executor gates"), nil
}

func (h *NMIProviderCutover) advanceAbandon(ctx context.Context, in gen.OpenrailsRailIntent, p nmiCutoverPayload, g nmiCutoverProgress, send bool) Outcome {
	evidence := func() map[string]any {
		b, _ := json.Marshal(g)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	unknown := func(reason string) Outcome { return AmbiguousWithEvidence(reason, evidence()) }
	if g.Target == nil || g.SourceCancelSubmitted || g.SourceCanceled || g.ActivationSubmitted || g.TargetActive {
		return unknown("cutover is no longer eligible for target abandonment")
	}
	source, target, err := h.abandonClients(ctx, in, p)
	if err != nil || !h.sourceRemainsActive(ctx, source, p) {
		return unknown("source identity or active state is unproven; target abandonment remains unresolved")
	}
	observed, active, err := target.GetCutoverSubscription(ctx, g.Target.ID)
	if err != nil || !cutoverSubscriptionTermsMatch(observed, p, g.Target.ID) {
		return unknown("target identity and frozen terms are unproven")
	}
	if active {
		paused, known := cutoverPaused(observed.PausedSubscription)
		if !known || !paused {
			return unknown("target became active; abandonment is refused")
		}
		if !send {
			return Retryable("paused target cancellation requires executor")
		}
		if target.ReadOnly {
			return Parked("provider writes are disabled")
		}
		if !g.TargetCancelSubmitted {
			claimed, err := NewStore(h.DB).RecordProgressIfAbsent(ctx, in.ID, "target_cancel_submitted", true)
			if err != nil || !claimed {
				return unknown("target cancellation submission already owned")
			}
			g.TargetCancelSubmitted = true
		}
		entered, writeErr := h.writeCutover(ctx, in, p, "target", target, func() error {
			return target.DeleteRecurringSubscription(ctx, g.Target.ID)
		})
		if !entered {
			return Parked("target cutover qualification is unavailable")
		}
		if writeErr != nil {
			return unknown("target cancellation outcome unknown: " + writeErr.Error())
		}
		observed, active, err = target.GetCutoverSubscription(ctx, g.Target.ID)
	}
	if err != nil || active || observed.DelayedCondition != "inactive" || !cutoverSubscriptionTermsMatch(observed, p, g.Target.ID) {
		return unknown("exact target cancellation receipt unavailable")
	}
	if !h.sourceRemainsActive(ctx, source, p) {
		return unknown("target canceled but source active state is unproven")
	}
	g.TargetCancelReceipt = &observed
	g.Abandoned = true
	return TerminalWithEvidence("cutover abandoned after exact target cancellation; source retained", evidence())
}
