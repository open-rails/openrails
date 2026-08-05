package intents

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/modules/money"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// TypeNMIDeleteSubscription is the deferred NMI delete_subscription intent
// (issue 216 / #344), migrated onto the ledger in #358 phase A. It replaces
// the NMIDeleteSubscription River job + boot rescan.
const TypeNMIDeleteSubscription = "nmi_delete_subscription"

// NMIDeletePayload is the stored payload for TypeNMIDeleteSubscription. The
// rail subscription id is re-read from the subscription row at execution
// time; the payload copy is evidence/forensics.
type NMIDeletePayload struct {
	UserID             string `json:"user_id"`
	RailSubscriptionID string `json:"rail_subscription_id,omitempty"`
}

// NMIDeleteIdempotencyKey is the logical identity of "the deferred delete of
// this subscription". Stable across re-cancels: a resume supersedes the
// intent and the next cancel's enqueue revives it.
func NMIDeleteIdempotencyKey(subscriptionID uuid.UUID) string {
	return TypeNMIDeleteSubscription + ":" + subscriptionID.String()
}

// NMIDeleteHandler implements verify-then-execute deletion of an NMI
// recurring subscription:
//
//   - relevance: the delete applies only while the subscription is still
//     cancelled with its DeletionScheduledAt marker set; a resume (status
//     active again) or an already-finalized delete supersedes the intent.
//   - execute: query the subscription at NMI first — absent IS success
//     (deletes are idempotent by observation); present -> delete. Any error
//     after the delete was sent is ambiguous (the verifier re-reads).
//   - kill switch (#344) and read-only clients park the intent pending with
//     the reason recorded, never failed.
//   - on success the DeletionScheduledAt read model is cleared (the
//     cancellation becomes destructive), exactly like the retired worker.
type NMIDeleteHandler struct {
	DB     *db.DB
	Config *config.Config
	// Resolver arms the intent merchant's NMI client from the armed rail
	// state at drain time (#788).
	Resolver money.NMIClientResolver
	Clock    clockwork.Clock
	Policy   BackoffPolicy
}

func NewNMIDeleteHandler(d *db.DB, cfg *config.Config, resolver money.NMIClientResolver, clock clockwork.Clock) *NMIDeleteHandler {
	return &NMIDeleteHandler{DB: d, Config: cfg, Resolver: resolver, Clock: clock, Policy: DefaultBackoff}
}

func (h *NMIDeleteHandler) Type() string { return TypeNMIDeleteSubscription }

func (h *NMIDeleteHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

func (h *NMIDeleteHandler) now() time.Time {
	if h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

// CheckRelevance: the deferred delete is applicable while the subscription is
// still cancelled with a pending deferred delete. This re-check at execution
// time is the AUTHORITATIVE resume guard (a missed advisory supersede on
// resume cannot cause an erroneous delete), mirroring the retired worker.
func (h *NMIDeleteHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status != models.StatusCancelled || sub.DeletionScheduledAt == nil {
		return SupersededBy(fmt.Sprintf("subscription no longer awaiting deferred delete (status=%s, marker_set=%t) — resumed or already finalized", sub.Status, sub.DeletionScheduledAt != nil)), nil
	}
	return StillRelevant(), nil
}

func (h *NMIDeleteHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil {
		return Parked("nmi rail not armable (fail closed): " + err.Error())
	}
	if !ok || client == nil {
		return Parked(fmt.Sprintf("nmi rail is not armed for provider %q", intent.Rail))
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}

	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		// Relevance passed moments ago; treat a read failure here as a clean
		// (no provider write attempted) retryable failure.
		return Retryable("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		// Nothing exists remotely to delete; finalize locally.
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("no rail subscription id, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"no_rail_subscription_id": true})
	}

	// Verify-then-execute: absent = success.
	present, err := h.subscriptionPresent(ctx, client, psid)
	if err != nil {
		return Retryable("provider read before delete failed: " + err.Error())
	}
	if !present {
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("verified absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_absent": true, "rail_subscription_id": psid})
	}

	if err := client.DeleteRecurringSubscription(ctx, psid); err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return Parked("nmi provider writes blocked (mode=readonly)")
		}
		// The delete request may or may not have reached NMI — never assume;
		// the verifier resolves by reading.
		return Ambiguous("delete_subscription failed: " + err.Error())
	}

	if err := h.finalize(ctx, intent); err != nil {
		// The provider delete DID happen; route through the verifier so
		// finalize is retried (its read will see the subscription absent).
		return Ambiguous("deleted at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{"deleted": true, "rail_subscription_id": psid})
}

// Verify resolves an ambiguous delete by reading the provider: absent means
// the delete (whenever it happened) is done; present means it definitely has
// not happened and the executor may retry.
func (h *NMIDeleteHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil || !ok || client == nil {
		return Ambiguous(fmt.Sprintf("nmi rail not armed for provider %q; cannot verify", intent.Rail))
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Ambiguous("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("no rail subscription id, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"no_rail_subscription_id": true})
	}
	present, err := h.subscriptionPresent(ctx, client, psid)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !present {
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("verified absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_absent": true, "rail_subscription_id": psid})
	}
	return Retryable("subscription still present at provider; delete verified not executed")
}

func (h *NMIDeleteHandler) loadSubscription(ctx context.Context, intent gen.OpenrailsRailIntent) (*models.Subscription, error) {
	if intent.SubscriptionID == nil {
		return nil, fmt.Errorf("intent has no subscription_id")
	}
	return subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, *intent.SubscriptionID)
}

// finalize clears the DeletionScheduledAt read model: the cancellation is now
// destructive (no longer resumable). Idempotent — a cleared marker is left
// alone.
func (h *NMIDeleteHandler) finalize(ctx context.Context, intent gen.OpenrailsRailIntent) error {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if db.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sub.DeletionScheduledAt == nil {
		return nil
	}
	sub.DeletionScheduledAt = nil
	return subscriptions.NewSubscriptionRepo(h.DB).UpdateAt(ctx, sub, h.now())
}

// subscriptionPresent reads GET /v5/subscriptions/{id}. NMI drops deleted/
// cancelled subscriptions entirely (the v5 GET answers 404), so "not found"
// means deleted.
func (h *NMIDeleteHandler) subscriptionPresent(ctx context.Context, client *nmi.NMIClient, railSubscriptionID string) (bool, error) {
	_, found, err := client.GetSubscription(ctx, railSubscriptionID)
	if err != nil {
		return false, err
	}
	return found, nil
}
