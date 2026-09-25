package intents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/railresolve"
)

// TypeStripeCancelSubscription stops Stripe billing a membership whose access
// OpenRails revoked (dispute, provider refund). Queued in the transaction that
// revokes access, so a revoked member is never left billing at Stripe.
const TypeStripeCancelSubscription = "stripe_cancel_subscription"

// StripeCancelPayload is the stored payload. The rail subscription id is
// re-read from the subscription row at execution time; the copy is evidence.
type StripeCancelPayload struct {
	RailSubscriptionID string `json:"rail_subscription_id"`
}

// StripeCancelIdempotencyKey: one remote cancel per subscription.
func StripeCancelIdempotencyKey(subscriptionID uuid.UUID) string {
	return TypeStripeCancelSubscription + ":" + subscriptionID.String()
}

// StripeOwned reports a subscription Stripe bills on its own schedule.
func StripeOwned(sub *models.Subscription) bool {
	return sub != nil && sub.Rail == models.RailStripe && sub.CollectionPolicy != models.CollectionPolicyEngine &&
		strings.TrimSpace(sub.RailSubscriptionID) != ""
}

// stripeCancelDenialMaxAttempts bounds retries of explicit auth refusals.
const stripeCancelDenialMaxAttempts = 3

// StripeCancelHandler verifies, then cancels:
//
//   - relevance: applies while the local subscription is still cancelled; a
//     won dispute that restores it supersedes the intent.
//   - execute: read first. Gone, canceled, or an active subscription already
//     set to cancel at period end is success. A paid-up subscription is set to
//     cancel at period end (reversible, and it never bills again); a
//     delinquent one is ended now, which also stops retries of its open
//     invoice.
//   - an uncertain write is resolved by the verifier's read, never resent blind.
type StripeCancelHandler struct {
	DB      *db.DB
	Config  *config.Config
	Rails   railresolve.Source
	Clients *stripeapi.Factory
	Policy  BackoffPolicy
}

func NewStripeCancelHandler(d *db.DB, cfg *config.Config, rails railresolve.Source, clients *stripeapi.Factory) *StripeCancelHandler {
	return &StripeCancelHandler{DB: d, Config: cfg, Rails: rails, Clients: clients, Policy: DefaultBackoff}
}

func (h *StripeCancelHandler) Type() string                         { return TypeStripeCancelSubscription }
func (h *StripeCancelHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

func (h *StripeCancelHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status != models.StatusCancelled {
		return SupersededBy(fmt.Sprintf("subscription no longer cancelled (status=%s)", sub.Status)), nil
	}
	return StillRelevant(), nil
}

func (h *StripeCancelHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	_, key, err := subscriptions.RequireStripeSecretKey(ctx, h.Rails)
	if err != nil {
		return Parked("stripe not configured: " + err.Error())
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
		return Retryable("stripe read before cancel failed: " + err.Error())
	}
	if stripeBillingStopped(rec) {
		return Succeeded(stripeCancelEvidence(psid, rec, false))
	}

	method, form, op := http.MethodDelete, url.Values(nil), "end"
	if rec.Status == "active" || rec.Status == "trialing" {
		method, form, op = http.MethodPost, url.Values{"cancel_at_period_end": {"true"}}, "cancel_at_period_end"
	}
	status, msg, err := h.write(ctx, key, method, psid, form, intent.IdempotencyKey+":"+op)
	switch {
	case errors.Is(err, stripeapi.ErrProviderReadOnly):
		return Parked("stripe provider writes blocked (mode=readonly)")
	case errors.Is(err, providerposture.ErrDisarmed):
		return Parked("stripe cancel held by sandbox posture gate: " + err.Error())
	case err != nil:
		return Ambiguous("stripe cancel outcome unknown: " + err.Error())
	case status < 300 || status == http.StatusNotFound:
		return Succeeded(map[string]any{"rail_subscription_id": psid, "action": op})
	case status == http.StatusTooManyRequests:
		return Retryable("stripe rate limited: " + msg)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if intent.Attempts >= stripeCancelDenialMaxAttempts {
			return Terminal(fmt.Sprintf("stripe refused the cancel after bounded retries (%d): %s", status, msg))
		}
		return Retryable(fmt.Sprintf("stripe refused the cancel (%d): %s", status, msg))
	default:
		// A 5xx may have applied; another 4xx may mean already canceled.
		return Ambiguous(fmt.Sprintf("stripe cancel answered %d: %s", status, msg))
	}
}

// Verify reads: billing stopped is success; still billing means the cancel
// definitely did not apply and may be retried.
func (h *StripeCancelHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
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
	if stripeBillingStopped(rec) {
		return Succeeded(stripeCancelEvidence(psid, rec, true))
	}
	return Retryable("subscription still billing at Stripe; cancel verified not applied")
}

// stripeBillingStopped: Stripe will not charge this subscription again.
func stripeBillingStopped(rec subscriptions.StripeLivenessRecord) bool {
	switch {
	case !rec.Found, rec.Status == "canceled", rec.Status == "incomplete_expired":
		return true
	case rec.Status == "active" || rec.Status == "trialing":
		return rec.CancelAtPeriodEnd
	default:
		return false
	}
}

func stripeCancelEvidence(psid string, rec subscriptions.StripeLivenessRecord, viaVerifier bool) map[string]any {
	ev := map[string]any{"rail_subscription_id": psid, "verified_billing_stopped": true, "stripe_status": rec.Status, "found": rec.Found}
	if rec.CancelAtPeriodEnd {
		ev["cancel_at_period_end"] = true
	}
	if viaVerifier {
		ev["via_verifier"] = true
	}
	return ev
}

func (h *StripeCancelHandler) probe(ctx context.Context, key, psid string) (subscriptions.StripeLivenessRecord, error) {
	p := &subscriptions.HTTPStripeLivenessProber{SecretKey: key, HTTPClient: h.Clients.ReadOnlyClient(30 * time.Second)}
	return p.ProbeSubscription(ctx, psid)
}

func (h *StripeCancelHandler) write(ctx context.Context, key, method, psid string, form url.Values, idempotencyKey string) (int, string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com/v1/subscriptions/"+url.PathEscape(psid), body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	stripeapi.SetIdempotencyKey(req, idempotencyKey)
	resp, err := h.Clients.Client(h.Config, 0).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, subscriptions.ParseStripeAPIError(raw), nil
}

func (h *StripeCancelHandler) loadSubscription(ctx context.Context, intent gen.OpenrailsRailIntent) (*models.Subscription, error) {
	if intent.SubscriptionID == nil {
		return nil, fmt.Errorf("intent has no subscription_id")
	}
	return subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, *intent.SubscriptionID)
}
