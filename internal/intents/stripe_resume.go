package intents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// TypeStripeResumeSubscription undoes the Stripe cancel of a membership its
// member or merchant resumed inside the paid period: Stripe bills it again
// (cancel_at_period_end false). Queued in the transaction that resumes the
// subscription.
const TypeStripeResumeSubscription = "stripe_resume_subscription"

// StripeResumePayload is the stored payload. The rail subscription id is
// re-read from the subscription row at execution time; the copy is evidence.
type StripeResumePayload struct {
	RailSubscriptionID string `json:"rail_subscription_id"`
}

// StripeResumeIdempotencyKey: one remote resume per cancellation undone,
// named by when the locked row was canceled.
func StripeResumeIdempotencyKey(subscriptionID uuid.UUID, canceledAt time.Time) string {
	return TypeStripeResumeSubscription + ":" + subscriptionID.String() + ":" + canceledAt.UTC().Format(time.RFC3339Nano)
}

// StripeResumeHandler reads, then resumes, through the cancel's Stripe reads
// and writes:
//
//   - relevance: applies while the local subscription is not canceled again;
//     a later cancel supersedes it.
//   - execute: read first. A subscription not set to cancel needs nothing; one
//     Stripe ended cannot be resumed (terminal). Otherwise
//     cancel_at_period_end is set false.
//   - an uncertain write is resolved by the verifier's read, never resent blind.
type StripeResumeHandler struct {
	StripeCancelHandler
}

func NewStripeResumeHandler(d *db.DB, cfg *billing.Config, rails railresolve.Source, clients *stripeapi.Factory, clock clockwork.Clock) *StripeResumeHandler {
	return &StripeResumeHandler{StripeCancelHandler{DB: d, Config: cfg, Rails: rails, Clients: clients, Clock: clock, Policy: DefaultBackoff}}
}

func (h *StripeResumeHandler) Type() string { return TypeStripeResumeSubscription }

func (h *StripeResumeHandler) CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status == models.StatusCanceled {
		return SupersededBy("subscription canceled again"), nil
	}
	return awaitEarlierStripeWrite(ctx, h.DB, intent, h.Clock)
}

func (h *StripeResumeHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	_, key, err := subscriptions.RequireStripeSecretKey(ctx, h.Rails)
	if err != nil {
		return parkedBeforeEffect("stripe not configured: " + err.Error())
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Retryable("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		return Succeeded(map[string]any{"no_rail_subscription_id": true})
	}
	rec, err := h.probe(ctx, key, psid)
	if err != nil {
		return Retryable("stripe read before resume failed: " + err.Error())
	}
	if outcome, settled := stripeResumeSettled(psid, rec); settled {
		return outcome
	}
	status, _, msg, err := h.write(ctx, key, http.MethodPost, psid, url.Values{"cancel_at_period_end": {"false"}}, intent.IdempotencyKey+":resume")
	switch {
	case errors.Is(err, stripeapi.ErrProviderReadOnly):
		return parkedBeforeEffect("stripe provider writes blocked (readonly write posture)")
	case errors.Is(err, providerposture.ErrDisarmed):
		return parkedBeforeEffect("stripe resume held by sandbox posture gate: " + err.Error())
	case err != nil:
		return Ambiguous("stripe resume outcome unknown: " + err.Error())
	case status < 300:
		return Succeeded(map[string]any{"rail_subscription_id": psid, "cancel_at_period_end": false})
	case status == http.StatusTooManyRequests:
		return Retryable("stripe rate limited: " + msg)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if intent.Attempts >= stripeCancelDenialMaxAttempts {
			return Terminal(fmt.Sprintf("stripe refused the resume after bounded retries (%d): %s", status, msg))
		}
		return Retryable(fmt.Sprintf("stripe refused the resume (%d): %s", status, msg))
	default:
		return Ambiguous(fmt.Sprintf("stripe resume answered %d: %s", status, msg))
	}
}

// Verify reads: billing on is success; still set to cancel means the resume
// did not apply and may be retried.
func (h *StripeResumeHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	_, key, err := subscriptions.RequireStripeSecretKey(ctx, h.Rails)
	if err != nil {
		return Ambiguous("stripe not configured; cannot verify: " + err.Error())
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Ambiguous("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		return Succeeded(map[string]any{"no_rail_subscription_id": true})
	}
	rec, err := h.probe(ctx, key, psid)
	if err != nil {
		return Ambiguous("stripe read failed: " + err.Error())
	}
	if outcome, settled := stripeResumeSettled(psid, rec); settled {
		return outcome
	}
	return Retryable("subscription still set to cancel at Stripe; resume verified not applied")
}

// stripeInvoiceWait is how long a Stripe cancel or resume waits for an earlier
// one of its subscription before it looks again.
const stripeInvoiceWait = time.Minute

// awaitEarlierStripeWrite keeps a Stripe cancel or resume unattempted while
// one of the same subscription committed before it is in flight or
// unresolved, so they reach Stripe in the order they were committed.
func awaitEarlierStripeWrite(ctx context.Context, d *db.DB, intent gen.BillingProviderIntent, clock clockwork.Clock) (Relevance, error) {
	if intent.SubscriptionID == nil {
		return StillRelevant(), nil
	}
	waiting, err := d.Gen(ctx).HasEarlierStripeScheduleWrite(ctx, gen.HasEarlierStripeScheduleWriteParams{
		MerchantID: intent.MerchantID, SubscriptionID: *intent.SubscriptionID, CreatedAt: intent.CreatedAt, ID: intent.ID,
	})
	if err != nil {
		return Relevance{}, fmt.Errorf("read earlier Stripe operations of the subscription: %w", err)
	}
	if waiting {
		return WaitUntil("an earlier Stripe cancel or resume of the subscription is unresolved", timeutil.FirstClock(clock).Now().UTC().Add(stripeInvoiceWait)), nil
	}
	return StillRelevant(), nil
}

// stripeResumeSettled is what a read alone answers: Stripe bills the
// subscription on (resumed), or ended it (nothing to resume).
func stripeResumeSettled(psid string, rec subscriptions.StripeLivenessRecord) (Outcome, bool) {
	switch {
	case !rec.Found, rec.Status == "canceled", rec.Status == "incomplete_expired":
		return TerminalWithEvidence("stripe ended the subscription; nothing to resume", map[string]any{"rail_subscription_id": psid, "stripe_status": rec.Status, "found": rec.Found}), true
	case !rec.CancelAtPeriodEnd:
		return Succeeded(map[string]any{"rail_subscription_id": psid, "stripe_status": rec.Status, "cancel_at_period_end": false}), true
	}
	return Outcome{}, false
}
