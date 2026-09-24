package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	log "github.com/sirupsen/logrus"
)

// FindingSubmissionUnresolved is a submitted engine charge the provider's
// reads cannot settle: the read is unavailable or contradictory, or the
// resend cap is spent. The member keeps the renewal allowance meanwhile.
const FindingSubmissionUnresolved = "life.submission.unresolved"

// lostSubmission decides a submitted charge the provider has no record of.
// Absence becomes the provider's answer only after the settle delay; the
// operation is then armed for one gated resend under the same reference.
func (h *SubscriptionCollectionHandler) lostSubmission(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload) intents.Outcome {
	history, err := intents.LoadSubmissionHistory(in)
	if err != nil {
		return h.unresolved(ctx, in, p, err.Error())
	}
	if !history.Settled(h.now()) {
		return intents.Ambiguous("the provider has no transaction yet; absence is conclusive after the settle delay")
	}
	if history.Resends >= intents.MaxLostSubmissionResends {
		return h.unresolved(ctx, in, p, fmt.Sprintf("the provider has no transaction after %d resends", history.Resends))
	}
	next := history.Resends + 1
	if history.Armed < next {
		if err := intents.NewStore(h.DB).ArmLostSubmissionResend(ctx, in, next); err != nil {
			return intents.Ambiguous("arm resend: " + err.Error())
		}
	}
	return intents.Retryable("the provider has no transaction for the submitted charge; resending under the same reference")
}

// armedResend returns the resend attempt the operation is armed for, or 0.
func armedResend(in gen.OpenrailsRailIntent) int {
	history, err := intents.LoadSubmissionHistory(in)
	if err != nil || history.Armed != history.Resends+1 {
		return 0
	}
	return history.Armed
}

// resendLostNMISubmission re-reads the order immediately before an armed
// resend; anything but a clean absence returns to verification.
func (h *SubscriptionCollectionHandler) resendLostNMISubmission(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload) intents.Outcome {
	attempt := armedResend(in)
	if attempt == 0 || p.Instrument.CustodianHeld() {
		return h.Verify(ctx, in)
	}
	if reason := h.submissionHeld(in); reason != "" {
		return intents.Parked(reason)
	}
	if attempts, err := intents.ReadNMIOrderAttempts(ctx, in, h.Resolver); err != nil || attempts.Transactions != 0 {
		return h.Verify(ctx, in)
	}
	method, _, _, err := h.validateAndFence(ctx, in, p, nil)
	if err != nil {
		return h.closeChangedResend(ctx, in, p, attempt, err)
	}
	charger, err := prepareEngineNMICharge(ctx, h.Resolver, method, p.HyperSwitch)
	if err != nil {
		return intents.Parked("arm accepted recurring charge: " + err.Error())
	}
	_, proof, first, err := h.validateAndFence(ctx, in, p, h.resendSubmission(in, attempt))
	if err != nil {
		return h.closeChangedResend(ctx, in, p, attempt, err)
	}
	if !first {
		return h.Verify(ctx, in)
	}
	return h.dispatchNMI(ctx, in, p, charger, proof)
}

// closeChangedResend ends an operation whose obligation changed while its
// lost submission waited: the provider showed nothing, so nothing executed.
func (h *SubscriptionCollectionHandler) closeChangedResend(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, attempt int, cause error) intents.Outcome {
	if !errors.Is(cause, errEngineObligationChanged) && !errors.Is(cause, charge.ErrInstrumentChanged) {
		return intents.Parked("fence engine resend: " + cause.Error())
	}
	proof, first, err := intents.NewStore(h.DB).BeginLostSubmissionResend(ctx, in, attempt, h.now())
	if err != nil || !first {
		return h.Verify(ctx, in)
	}
	return h.completeNotExecuted(ctx, in, p, "instrument_changed", cause.Error(), proof)
}

// unresolved keeps the operation unknown. Once the settle delay has passed it
// is also a standing operator finding, closed when the operation completes.
func (h *SubscriptionCollectionHandler) unresolved(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, reason string) intents.Outcome {
	if history, err := intents.LoadSubmissionHistory(in); err != nil || history.Settled(h.now()) {
		evidence, _ := json.Marshal(map[string]any{"operation_id": in.ID, "subscription_id": p.Renewal.SubscriptionID, "rail": in.Rail, "order_reference": p.OrderReference, "reason": reason})
		action := fmt.Sprintf("a submitted renewal charge for subscription %s cannot be settled from the provider (%s). No further charge is sent while this stands; confirm the charge at the provider and resolve the operation", p.Renewal.SubscriptionID, reason)
		if _, err := h.DB.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID: in.MerchantID, FindingType: FindingSubmissionUnresolved, SubjectKey: in.ID.String(),
			Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
		}); err != nil {
			log.WithContext(ctx).WithError(err).WithField("operation_id", in.ID).Error("engine collection: could not record the unresolved-submission finding")
		}
	}
	return intents.Ambiguous(reason)
}
