package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	log "github.com/sirupsen/logrus"
)

// FindingSubmissionUnresolved is a submitted engine charge the provider's
// reads cannot settle: the read is unavailable or contradictory, or the
// Stripe resend cap is spent. The member keeps the renewal allowance meanwhile.
const FindingSubmissionUnresolved = "life.submission.unresolved"

// lostSubmission keeps ambiguous NMI submissions pending. The Query API has
// no documented visibility bound, and dup_seconds is processor-dependent;
// absence cannot prove nonexecution. Stripe alone can replay its original
// provider idempotency key inside its retention window.
func (h *SubscriptionCollectionHandler) lostSubmission(ctx context.Context, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload) intents.Outcome {
	history, err := intents.LoadSubmissionHistory(in)
	if err != nil {
		return h.unresolved(ctx, in, p, err.Error())
	}
	if !history.Settled(h.now()) {
		return intents.Ambiguous("the provider has no transaction yet; waiting for receipt visibility")
	}
	if refused, ok := intents.DuplicateRefusedAt(in); ok {
		return h.duplicateUnresolved(ctx, in, p, refused)
	}
	if in.Rail != "stripe" {
		return h.unresolved(ctx, in, p, "NMI has no visible transaction for the submitted charge; provider evidence is required, never an automatic resend")
	}
	if history.Resends >= intents.MaxLostSubmissionResends {
		return h.unresolved(ctx, in, p, fmt.Sprintf("the provider has no transaction after %d resends", history.Resends))
	}
	if !history.StripeReplaySafe(h.now()) {
		return h.unresolved(ctx, in, p, "Stripe idempotency retention window elapsed; provider receipt required")
	}
	next := history.Resends + 1
	if history.Armed < next {
		if err := intents.NewStore(h.DB).ArmLostSubmissionResend(ctx, in, next); err != nil {
			return intents.Ambiguous("arm resend: " + err.Error())
		}
	}
	return intents.Retryable("the provider has no transaction for the submitted charge; resending under the same order")
}

// duplicateLookback bounds the vault read after a duplicate refusal: NMI's
// window is an account setting, so the read reaches well past any default.
const duplicateLookback = 24 * time.Hour

// duplicateUnresolved answers a duplicate refusal with nothing under this
// obligation's order: the matching charge is another order's or not yet
// visible, so the operation stays unknown and is never resent. The finding
// names the vault's matching charges for the operator.
func (h *SubscriptionCollectionHandler) duplicateUnresolved(ctx context.Context, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, refused time.Time) intents.Outcome {
	txns, err := intents.ReadNMIVaultTransactions(ctx, in, h.Resolver, refused.Add(-duplicateLookback))
	if err != nil {
		return h.unresolved(ctx, in, p, "NMI refused the charge as a duplicate and the vault read is inconclusive: "+err.Error())
	}
	var matches []string
	for _, txn := range txns {
		if txn.ApprovedMinor == int64(p.AmountMinor) {
			if txn.OrderID == p.OrderReference {
				// Indexed after the order read: the receipt read qualifies it.
				return intents.Ambiguous("the vault shows this obligation's charge; verifying its receipt")
			}
			matches = append(matches, txn.TransactionID+" (order "+txn.OrderID+")")
		}
	}
	if len(matches) == 0 {
		return h.unresolved(ctx, in, p, "NMI refused the charge as a duplicate but the vault shows no matching charge")
	}
	return h.unresolved(ctx, in, p, "NMI refused the charge as a duplicate of another order's charge: "+strings.Join(matches, ", "))
}

// armedResend returns the resend attempt the operation is armed for, or 0.
func armedResend(in gen.BillingProviderIntent) int {
	history, err := intents.LoadSubmissionHistory(in)
	if err != nil || history.Armed != history.Resends+1 {
		return 0
	}
	return history.Armed
}

// closeChangedResend ends a Stripe retry whose obligation changed after its
// provider lookup returned no payment inside the key retention window.
func (h *SubscriptionCollectionHandler) closeChangedResend(ctx context.Context, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, attempt int, cause error) intents.Outcome {
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
func (h *SubscriptionCollectionHandler) unresolved(ctx context.Context, in gen.BillingProviderIntent, p subscriptions.SubscriptionCollectionPayload, reason string) intents.Outcome {
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
