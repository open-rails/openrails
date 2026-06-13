package intents

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// TypeNMIDeleteSubscription is the deferred NMI delete_subscription intent
// (issue 216 / #344), migrated onto the ledger in #358 phase A. It replaces
// the NMIDeleteSubscription River job + boot rescan.
const TypeNMIDeleteSubscription = "nmi_delete_subscription"

// NMIDeletePayload is the stored payload for TypeNMIDeleteSubscription. The
// processor subscription id is re-read from the subscription row at execution
// time; the payload copy is evidence/forensics.
type NMIDeletePayload struct {
	UserID                  string `json:"user_id"`
	ProcessorSubscriptionID string `json:"processor_subscription_id,omitempty"`
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
	DB      *db.DB
	Config  *config.Config
	Clients map[string]*nmi.NMIClient
	Clock   clockwork.Clock
	Policy  BackoffPolicy
}

func NewNMIDeleteHandler(d *db.DB, cfg *config.Config, clients map[string]*nmi.NMIClient, clock clockwork.Clock) *NMIDeleteHandler {
	return &NMIDeleteHandler{DB: d, Config: cfg, Clients: clients, Clock: clock, Policy: DefaultBackoff}
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
func (h *NMIDeleteHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsProviderIntent) (Relevance, error) {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if repo.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status != models.StatusCancelled || sub.DeletionScheduledAt == nil {
		return SupersededBy(fmt.Sprintf("subscription no longer awaiting deferred delete (status=%s, marker_set=%t) — resumed or already finalized", sub.Status, sub.DeletionScheduledAt != nil)), nil
	}
	return StillRelevant(), nil
}

func (h *NMIDeleteHandler) Execute(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	client, ok := h.Clients[strings.ToLower(intent.Provider)]
	if !ok || client == nil {
		return Parked(fmt.Sprintf("nmi client not configured for provider %q", intent.Provider))
	}
	// Kill switch (#344): checked at execution time, before any provider
	// call. The intent stays pending and drains when the flag lifts.
	if client.SubscriptionDeletesDisabled || (h.Config != nil && h.Config.IsProcessorSubscriptionDeletionDisabled()) {
		return Parked("processor subscription deletes disabled (feature_flags.disable_processor_subscription_deletions kill switch)")
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
	psid := strings.TrimSpace(sub.ProcessorSubscriptionID)
	if psid == "" {
		// Nothing exists remotely to delete; finalize locally.
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("no processor subscription id, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"no_processor_subscription_id": true})
	}

	// Verify-then-execute: absent = success.
	present, err := h.subscriptionPresent(client, psid)
	if err != nil {
		return Retryable("provider read before delete failed: " + err.Error())
	}
	if !present {
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("verified absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_absent": true, "processor_subscription_id": psid})
	}

	if err := client.DeleteRecurringSubscription(psid); err != nil {
		if errors.Is(err, nmi.ErrSubscriptionDeletesDisabled) {
			return Parked("processor subscription deletes disabled (feature_flags.disable_processor_subscription_deletions kill switch)")
		}
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
	return Succeeded(map[string]any{"deleted": true, "processor_subscription_id": psid})
}

// Verify resolves an ambiguous delete by reading the provider: absent means
// the delete (whenever it happened) is done; present means it definitely has
// not happened and the executor may retry.
func (h *NMIDeleteHandler) Verify(ctx context.Context, intent gen.OpenrailsProviderIntent) Outcome {
	client, ok := h.Clients[strings.ToLower(intent.Provider)]
	if !ok || client == nil {
		return Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Provider))
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Ambiguous("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.ProcessorSubscriptionID)
	if psid == "" {
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("no processor subscription id, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"no_processor_subscription_id": true})
	}
	present, err := h.subscriptionPresent(client, psid)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !present {
		if err := h.finalize(ctx, intent); err != nil {
			return Ambiguous("verified absent at provider, but local finalize failed: " + err.Error())
		}
		return Succeeded(map[string]any{"verified_absent": true, "processor_subscription_id": psid})
	}
	return Retryable("subscription still present at provider; delete verified not executed")
}

func (h *NMIDeleteHandler) loadSubscription(ctx context.Context, intent gen.OpenrailsProviderIntent) (*models.Subscription, error) {
	if intent.SubscriptionID == nil {
		return nil, fmt.Errorf("intent has no subscription_id")
	}
	return repo.NewSubscriptionRepo(h.DB).GetByID(ctx, *intent.SubscriptionID)
}

// finalize clears the DeletionScheduledAt read model: the cancellation is now
// destructive (no longer resumable). Idempotent — a cleared marker is left
// alone.
func (h *NMIDeleteHandler) finalize(ctx context.Context, intent gen.OpenrailsProviderIntent) error {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sub.DeletionScheduledAt == nil {
		return nil
	}
	sub.DeletionScheduledAt = nil
	return repo.NewSubscriptionRepo(h.DB).UpdateAt(ctx, sub, h.now())
}

// nmiRecurringQueryResponse mirrors the slice of the NMI Query API recurring
// report needed to answer "does this subscription still exist?".
type nmiRecurringQueryResponse struct {
	XMLName       xml.Name `xml:"nm_response"`
	Subscriptions []struct {
		SubscriptionID string `xml:"subscription_id"`
	} `xml:"subscription"`
	ErrorResponse string `xml:"error_response"`
}

// subscriptionPresent reads the recurring report filtered by subscription_id.
// NMI drops deleted/cancelled subscriptions from the report entirely, so "no
// matching row" means deleted.
func (h *NMIDeleteHandler) subscriptionPresent(client *nmi.NMIClient, processorSubscriptionID string) (bool, error) {
	raw, err := client.QueryRecurringSubscriptions(nmi.RecurringQueryParams{SubscriptionID: processorSubscriptionID})
	if err != nil {
		return false, err
	}
	var parsed nmiRecurringQueryResponse
	if err := xml.Unmarshal([]byte(raw), &parsed); err != nil {
		return false, fmt.Errorf("parse recurring query response: %w", err)
	}
	if parsed.ErrorResponse != "" {
		return false, fmt.Errorf("recurring query error_response: %s", parsed.ErrorResponse)
	}
	for _, s := range parsed.Subscriptions {
		if strings.TrimSpace(s.SubscriptionID) == strings.TrimSpace(processorSubscriptionID) {
			return true, nil
		}
	}
	return false, nil
}
