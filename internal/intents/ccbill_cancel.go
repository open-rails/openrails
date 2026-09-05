package intents

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
)

// Only explicit HTTP/authentication refusals permit bounded clean retries.
// Provider code -7 remains ambiguous and goes through cancellation verification.
const ccbillDenialMaxAttempts = 3

func ccbillDenialExhausted(attempts int32) bool { return attempts >= ccbillDenialMaxAttempts }

// TypeCCBillCancelSubscription is the merchant-initiated CCBill cancel intent
// (#696): stop future rebills via DataLink's SMS cancelSubscription; CCBill
// keeps the subscriber's access through the paid period on its own side, and
// the local cancel (recorded in the SAME tx as this enqueue) already carries
// the #691 paid-runway closure.
const TypeCCBillCancelSubscription = "ccbill_cancel_subscription"

// CCBillCancelPayload is the stored payload. The rail subscription id is
// re-read from the subscription row at execution time; the copy is evidence.
type CCBillCancelPayload struct {
	UserID             string `json:"user_id"`
	RailSubscriptionID string `json:"rail_subscription_id,omitempty"`
}

// CCBillCancelIdempotencyKey: the logical identity of "the remote cancel of
// this subscription" — repeated cancels refresh the one pending intent.
func CCBillCancelIdempotencyKey(subscriptionID uuid.UUID) string {
	return TypeCCBillCancelSubscription + ":" + subscriptionID.String()
}

// CCBillCancelHandler implements verify-then-execute cancellation of a CCBill
// subscription through the DataLink SMS choke point:
//
//   - relevance: applies while the local subscription is still cancelled; a
//     reactivation (CCBill UserReactivation, admin repair) supersedes it.
//   - execute: viewSubscriptionStatus FIRST — already not-rebilling IS success
//     (cancels are idempotent by observation); rebilling -> cancelSubscription.
//     Any definite reject or transport failure after the send is ambiguous:
//     the verifier re-reads (verify-not-decline, #674).
//   - mode: the runner's origin x mode gate parks first; the client-level
//     ReadOnly transport gate is the backstop (parked, never failed).
type CCBillCancelHandler struct {
	DB     *db.DB
	Config *config.Config
	// Rails arms the intent merchant's DataLink client from the armed rail
	// state at drain time (#788).
	Rails railresolve.Source
	// DataLinkBaseURL overrides the DataLink endpoint (test seam).
	DataLinkBaseURL string
	Clock           clockwork.Clock
	Policy          BackoffPolicy
}

func NewCCBillCancelHandler(d *db.DB, cfg *config.Config, rails railresolve.Source, clock clockwork.Clock) *CCBillCancelHandler {
	return &CCBillCancelHandler{DB: d, Config: cfg, Rails: rails, Clock: clock, Policy: DefaultBackoff}
}

func (h *CCBillCancelHandler) Type() string { return TypeCCBillCancelSubscription }

func (h *CCBillCancelHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// CheckRelevance: the remote cancel applies while the subscription is still
// locally cancelled. Re-checked at execution time so a reactivation between
// enqueue and drain can never cancel a live subscription remotely.
func (h *CCBillCancelHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status != models.StatusCancelled {
		return SupersededBy(fmt.Sprintf("subscription no longer cancelled (status=%s) — reactivated", sub.Status)), nil
	}
	return StillRelevant(), nil
}

func (h *CCBillCancelHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, err := ccbillDataLinkForMerchant(ctx, h.Config, h.Rails, h.DataLinkBaseURL)
	if err != nil {
		return Parked("ccbill rail not armable (fail closed): " + err.Error())
	}
	if client.ReadOnly {
		return Parked("ccbill datalink client is read-only (mode=readonly)")
	}

	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Retryable("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		// Nothing exists remotely to cancel.
		return Succeeded(map[string]any{"no_rail_subscription_id": true})
	}

	// Verify-then-execute: already not-rebilling = success.
	rebilling, status, err := h.rebilling(ctx, client, psid)
	if err != nil {
		// Read failed cleanly — no write attempted. Covers auth failures and
		// the (Phase-0-uncaptured) unknown-subscription answer: retried, never
		// resolved off a guess.
		return Retryable("provider read before cancel failed: " + err.Error())
	}
	if !rebilling {
		return Succeeded(verifiedEvidence(psid, status, false))
	}

	res, err := client.CancelSubscription(ctx, psid)
	if err != nil {
		switch {
		case errors.Is(err, ccbill.ErrProviderReadOnly):
			return Parked("ccbill provider writes blocked (mode=readonly)")
		case errors.Is(err, ccbill.ErrDataLinkAuth):
			// An explicit HTTP/authentication rejection did not execute the action.
			if ccbillDenialExhausted(intent.Attempts) {
				return Terminal("cancelSubscription authentication/access rejected: provider refused after bounded retries — not permitted / auth — needs operator attention")
			}
			return Retryable("cancelSubscription authentication/access rejected: provider refused (auth/IP, or not permitted); bounded retry")
		default:
			// Definite reject (may mean already-cancelled) or transport
			// ambiguity — the verifier's read resolves either way.
			return Ambiguous("cancelSubscription failed: " + err.Error())
		}
	}
	return Succeeded(map[string]any{"cancelled": true, "results": res.Results, "rail_subscription_id": psid})
}

// Verify resolves an ambiguous cancel by reading: not-rebilling means the
// cancel (whenever it happened) is done; still-rebilling means it definitely
// has not happened and the executor may retry.
func (h *CCBillCancelHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, cerr := ccbillDataLinkForMerchant(ctx, h.Config, h.Rails, h.DataLinkBaseURL)
	if cerr != nil {
		return Ambiguous("ccbill rail not armable; cannot verify: " + cerr.Error())
	}
	sub, err := h.loadSubscription(ctx, intent)
	if err != nil {
		return Ambiguous("load subscription: " + err.Error())
	}
	psid := strings.TrimSpace(sub.RailSubscriptionID)
	if psid == "" {
		return Succeeded(map[string]any{"no_rail_subscription_id": true})
	}
	rebilling, status, err := h.rebilling(ctx, client, psid)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !rebilling {
		return Succeeded(verifiedEvidence(psid, status, true))
	}
	return Retryable("subscription still rebilling at provider; cancel verified not executed")
}

// rebilling reads the provider state. An unrecognized status vocabulary value
// is an error (#651: undecidable, never guessed).
func (h *CCBillCancelHandler) rebilling(ctx context.Context, client *ccbill.DataLinkClient, psid string) (bool, ccbill.SubscriptionStatusResult, error) {
	status, err := client.ViewSubscriptionStatus(ctx, psid)
	if err != nil {
		return false, status, err
	}
	rebilling, err := status.Rebilling()
	if err != nil {
		return false, status, err
	}
	return rebilling, status, nil
}

func verifiedEvidence(psid string, status ccbill.SubscriptionStatusResult, viaVerifier bool) map[string]any {
	ev := map[string]any{
		"verified_not_rebilling": true,
		"raw_status":             status.RawStatus,
		"rail_subscription_id":   psid,
	}
	if viaVerifier {
		ev["via_verifier"] = true
	}
	if exp, ok := status.ExpiresAt(); ok {
		// CCBill's own final paid-through instant — forensics for the #691
		// runway, recorded verbatim, never written onto the local row here.
		ev["provider_expires_at"] = exp.Format(time.RFC3339)
	}
	return ev
}

func (h *CCBillCancelHandler) loadSubscription(ctx context.Context, intent gen.OpenrailsRailIntent) (*models.Subscription, error) {
	if intent.SubscriptionID == nil {
		return nil, fmt.Errorf("intent has no subscription_id")
	}
	return subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, *intent.SubscriptionID)
}

// CCBillCancelScheduler implements subscriptions.CCBillRemoteCancelScheduler
// on the intent ledger: QUEUE-ALWAYS (#679 — mode/credentials gate execution
// only), due immediately (no undo window: CCBill preserves paid access on its
// own side, so there is nothing to defer for).
type CCBillCancelScheduler struct {
	db     *db.DB
	store  *Store
	origin Origin
	reason string
}

// NewCCBillCancelScheduler builds the scheduler. ceiling (may be nil) is the
// #732 rate ceiling; user/admin-origin schedulers pass it so self-service and
// admin CCBill cancels are gated.
func NewCCBillCancelScheduler(d *db.DB, ceiling *RateCeiling, origin Origin, reason string) *CCBillCancelScheduler {
	return &CCBillCancelScheduler{db: d, store: NewStoreGated(d, ceiling), origin: origin, reason: reason}
}

// WithTx rebinds the scheduler onto the caller's transaction so the intent
// enqueue commits atomically with the local cancellation. The rate ceiling
// (its own pool-backed DB) is preserved across the rebind.
func (s *CCBillCancelScheduler) WithTx(tx pgx.Tx) subscriptions.CCBillRemoteCancelScheduler {
	if s == nil {
		return s
	}
	txdb := s.db.NewWithPgxTx(tx)
	return &CCBillCancelScheduler{db: txdb, store: s.store.withTxDB(txdb), origin: s.origin, reason: s.reason}
}

// ScheduleCCBillCancel enqueues the durable remote cancel, due now.
// Idempotent per subscription (intents idempotency_key).
func (s *CCBillCancelScheduler) ScheduleCCBillCancel(ctx context.Context, userID string, subscriptionID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("intent ledger unavailable for ccbill cancel scheduling")
	}
	sub, err := s.db.Gen(ctx).GetSubscriptionByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("load subscription for ccbill cancel intent: %w", err)
	}
	_, err = s.store.Enqueue(ctx, EnqueueParams{
		MerchantID:     sub.MerchantID,
		Provider:       strings.ToLower(sub.Rail),
		IntentType:     TypeCCBillCancelSubscription,
		SubscriptionID: &subscriptionID,
		// or#893: the cancel is addressed to the account that holds the
		// subscription, which the row itself names.
		PspID: sub.PspID,
		Payload: CCBillCancelPayload{
			UserID:             userID,
			RailSubscriptionID: sub.RailSubscriptionID,
		},
		IdempotencyKey: CCBillCancelIdempotencyKey(subscriptionID),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         s.origin,
		OriginReason:   s.reason,
	})
	return err
}
