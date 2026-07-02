package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Refund intent types (#358 phase B). The provider-side money movement of an
// admin refund flows through the ledger; the local reservation flow around it
// is unchanged: the producer reserves (negative pending payment row), the
// handler's finalize completes the reservation with the provider refund id on
// success — owning completion so the async drain repairs a producer that died
// mid-call — and releases it (status=failed) on a terminal refusal.
const (
	TypeNMIRefund    = "nmi_refund"
	TypeStripeRefund = "stripe_refund"
)

// RefundPayload is the stored payload for refund intents.
type RefundPayload struct {
	OriginalPaymentID uuid.UUID `json:"original_payment_id"`
	// ReservationID is the local negative pending payment row the finalize
	// completes (provider id recorded) or releases (terminal refusal).
	ReservationID uuid.UUID `json:"reservation_id"`
	// AmountCents is provider minor units (typed CENTS, #671) — converted
	// exactly from the request's micros at the admin boundary. Both the NMI
	// wire amount and the verifier's action.AmountCents comparison are cents.
	AmountCents moneyutil.Cents `json:"amount_cents"`
	Reason      string          `json:"reason,omitempty"`
	// ProviderTarget is what the provider refunds against: the Stripe charge /
	// payment-intent id (ch_/pi_), or the NMI transaction id of the original
	// payment.
	ProviderTarget string `json:"provider_target"`
}

// NMIRefundIdempotencyKey content-addresses one logical NMI refund: original
// transaction + amount. Re-asking for the same refund is ONE provider
// mutation (NMI has no request-level idempotency; this key is the guard).
func NMIRefundIdempotencyKey(transactionID string, amountCents int64) string {
	return fmt.Sprintf("%s:%s:%d", TypeNMIRefund, strings.TrimSpace(transactionID), amountCents)
}

// StripeRefundIntentIdempotencyKey is the stripe_refund intent identity —
// deliberately the SAME content-addressed key the Stripe Idempotency-Key
// header carries (charge + amount + reason), so ledger dedupe and provider
// dedupe agree on what "the same refund" means.
func StripeRefundIntentIdempotencyKey(chargeID string, amountCents int64, reason string) string {
	return subscriptions.StripeRefundIdempotencyKey(chargeID, amountCents, reason)
}

// refundReservations is the shared local-reservation surface of both refund
// handlers.
type refundReservations struct {
	DB    *db.DB
	Clock clockwork.Clock
}

func (r refundReservations) payments() *payments.PaymentService {
	return payments.NewPaymentService(r.DB, r.Clock)
}

func decodeRefundPayload(intent gen.OpenrailsRailIntent) (RefundPayload, error) {
	var p RefundPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("refund intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode refund payload: %w", err)
	}
	if p.ReservationID == uuid.Nil || strings.TrimSpace(p.ProviderTarget) == "" || p.AmountCents <= 0 {
		return p, errors.New("refund payload is incomplete (reservation, provider target and amount are required)")
	}
	return p, nil
}

// checkRelevance: a refund intent applies while its local reservation is
// still open (pending). A completed reservation means the refund already
// finalized; a released/failed or deleted one means it was abandoned.
func (r refundReservations) checkRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := decodeRefundPayload(intent)
	if err != nil {
		// Malformed payloads can never become executable; superseding surfaces
		// them in reconcile instead of re-parking forever.
		return SupersededBy("unusable refund intent: " + err.Error()), nil
	}
	reservation, err := r.payments().GetByID(ctx, p.ReservationID)
	if err != nil {
		if repo.IsNotFound(err) {
			return SupersededBy("refund reservation no longer exists"), nil
		}
		return Relevance{}, err
	}
	switch strings.ToLower(strings.TrimSpace(reservation.Status)) {
	case payments.PaymentStatusPendingValue:
		return StillRelevant(), nil
	case payments.PaymentStatusCompletedValue, "":
		return SupersededBy("refund reservation already completed"), nil
	default:
		return SupersededBy(fmt.Sprintf("refund reservation released (status=%s)", reservation.Status)), nil
	}
}

// finalize completes the reservation with the provider refund id. Idempotent:
// an already-completed reservation is left alone. Metadata is carried over
// from the reservation (CompleteRefundReservation replaces it wholesale) with
// the completion stamped in.
func (r refundReservations) finalize(ctx context.Context, p RefundPayload, providerRefundID string) error {
	svc := r.payments()
	reservation, err := svc.GetByID(ctx, p.ReservationID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil
		}
		return err
	}
	if payments.PaymentStatusCompleted(reservation.Status) {
		return nil
	}
	metadata := map[string]any{}
	for k, v := range reservation.Metadata {
		metadata[k] = v
	}
	metadata["admin_refund_status"] = "completed"
	metadata["provider_refund_id"] = providerRefundID
	_, err = svc.CompleteRefundReservation(ctx, p.ReservationID, providerRefundID, metadata)
	return err
}

// release cancels the reservation after a terminal provider refusal.
func (r refundReservations) release(ctx context.Context, p RefundPayload) error {
	return r.payments().MarkFailed(ctx, p.ReservationID)
}

// terminally releases the reservation and reports the terminal outcome; a
// failed release keeps the outcome ambiguous so the lease/verifier retries
// the release rather than stranding an open reservation on a dead intent.
func (r refundReservations) terminally(ctx context.Context, p RefundPayload, reason string, evidence map[string]any) Outcome {
	if err := r.release(ctx, p); err != nil {
		return Ambiguous(reason + " (and reservation release failed: " + err.Error() + ")")
	}
	return TerminalWithEvidence(reason, evidence)
}

// ============================================================================
// NMI
// ============================================================================

// NMIRefundHandler executes refunds against NMI-backed rails. NMI has no
// request-level idempotency, so effectively-once rests on the ledger: one
// intent per (transaction, amount), never blind-retried — any outcome that
// MIGHT have moved money parks as unknown_needs_verify and the verifier
// resolves it by reading the transaction's refund actions off the Query API.
type NMIRefundHandler struct {
	refundReservations
	Clients map[string]*nmi.NMIClient
	Policy  BackoffPolicy
}

func NewNMIRefundHandler(d *db.DB, clients map[string]*nmi.NMIClient, clock clockwork.Clock) *NMIRefundHandler {
	return &NMIRefundHandler{
		refundReservations: refundReservations{DB: d, Clock: clock},
		Clients:            clients,
		Policy:             DefaultBackoff,
	}
}

func (h *NMIRefundHandler) Type() string                         { return TypeNMIRefund }
func (h *NMIRefundHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy keeps the payload on a succeeded refund tombstone (#607): the
// admin refund producer reads reservation_id off the durable succeeded row to
// detect a double-refund conflict (admin_payments.go). The forensic evidence
// (provider_refund_id) is slimmed — the completed reservation row, not the
// intent, is the source of truth post-success.
func (h *NMIRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func (h *NMIRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *NMIRefundHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, ok := h.Clients[strings.ToLower(intent.Provider)]
	if !ok || client == nil {
		return Parked(fmt.Sprintf("nmi client not configured for provider %q", intent.Provider))
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}

	// Money mover: a reclaimed lease / verified-not-executed retry means a
	// prior execution may have reached the gateway. Verify by reading before
	// sending another refund.
	if intent.Attempts > 1 {
		refundTxnID, found, verr := h.findRefund(client, p)
		if verr != nil {
			return Ambiguous("pre-send verification read failed: " + verr.Error())
		}
		if found {
			if err := h.finalize(ctx, p, refundTxnID); err != nil {
				return Ambiguous("refund verified at provider, but local finalize failed: " + err.Error())
			}
			return Succeeded(map[string]any{"provider_refund_id": refundTxnID, "verified_existing": true})
		}
	}

	result, err := client.Refund(nmi.RefundParams{TransactionID: p.ProviderTarget, Amount: p.AmountCents})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return Parked("nmi provider writes blocked (mode=readonly)")
		}
		var vaultErr *nmi.CustomerVaultError
		if errors.As(err, &vaultErr) {
			// The gateway answered with a decline: the refund definitively did
			// not happen and re-sending the identical request cannot succeed.
			return h.terminally(ctx, p, "nmi refund declined: "+vaultErr.Message, map[string]any{
				"response_code": vaultErr.ResponseCode,
			})
		}
		// Transport-level failure: the refund MAY have been processed.
		return Ambiguous("nmi refund outcome unknown: " + err.Error())
	}

	if err := h.finalize(ctx, p, result.TransactionID); err != nil {
		return Ambiguous("refunded at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{"provider_refund_id": result.TransactionID})
}

// Verify resolves an ambiguous NMI refund via the Query API: a successful
// refund/credit action of the intent's amount against the original
// transaction means the money moved (finalize + succeed); a clean read with
// no such action means it definitively did not (the executor may retry).
func (h *NMIRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, ok := h.Clients[strings.ToLower(intent.Provider)]
	if !ok || client == nil {
		return Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Provider))
	}
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	refundTxnID, found, err := h.findRefund(client, p)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Retryable("no refund of this amount found at provider; verified not executed")
	}
	if err := h.finalize(ctx, p, refundTxnID); err != nil {
		return Ambiguous("refund verified at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{"provider_refund_id": refundTxnID, "verified_existing": true})
}

// findRefund reads the original transaction (GET /v5/payments/{id}) and looks
// for a successful refund/credit action matching the intent amount. The
// returned id is the action's own transaction id when NMI reports one, else
// the original's.
func (h *NMIRefundHandler) findRefund(client *nmi.NMIClient, p RefundPayload) (refundTransactionID string, found bool, err error) {
	actions, txnFound, err := client.GetPaymentActions(p.ProviderTarget)
	if err != nil {
		return "", false, err
	}
	if !txnFound {
		return "", false, fmt.Errorf("transaction %s not found at provider", p.ProviderTarget)
	}
	for _, action := range actions {
		if action.Type != "refund" && action.Type != "credit" {
			continue
		}
		if !action.Success {
			continue
		}
		if moneyutil.Cents(action.AmountCents) != p.AmountCents {
			continue
		}
		return action.TransactionID, true, nil
	}
	return "", false, nil
}

// nmiAmountToCents converts the Query API dollar amount ("5.00", "-5.00" on
// refund actions) into absolute cents.
func nmiAmountToCents(amount string) (int64, error) {
	trimmed := strings.TrimSpace(amount)
	if trimmed == "" {
		return 0, errors.New("empty amount")
	}
	f, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		f = -f
	}
	return int64(f*100 + 0.5), nil
}

// ============================================================================
// Stripe
// ============================================================================

// stripeRefundAPI is the StripeRefundService slice the handler drives
// (interface for unit tests).
type stripeRefundAPI interface {
	CreateRefund(ctx context.Context, params subscriptions.RefundParams) (*subscriptions.RefundResult, error)
	FindRefundByIdempotencyKey(ctx context.Context, chargeID, idempotencyKey string) (*subscriptions.RefundResult, bool, error)
}

// StripeRefundHandler executes refunds against Stripe. Effectively-once is
// double-walled: the ledger dedupes intents AND every create carries the
// intent's idempotency_key as the Stripe Idempotency-Key header, so even a
// raced duplicate POST replays the original refund instead of minting a
// second one. The verifier re-finds refunds via reads (GET /v1/refunds,
// matched on the metadata mirror of the key).
type StripeRefundHandler struct {
	refundReservations
	Config *config.Config
	Rails  config.RailMerchantAccountSet
	Stripe stripeRefundAPI
	Policy BackoffPolicy
}

func NewStripeRefundHandler(d *db.DB, cfg *config.Config, rails config.RailMerchantAccountSet, clock clockwork.Clock) *StripeRefundHandler {
	return &StripeRefundHandler{
		refundReservations: refundReservations{DB: d, Clock: clock},
		Config:             cfg,
		Rails:              rails,
		Stripe:             &subscriptions.StripeRefundService{Config: cfg, Rails: rails},
		Policy:             DefaultBackoff,
	}
}

func (h *StripeRefundHandler) Type() string                         { return TypeStripeRefund }
func (h *StripeRefundHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy keeps the payload on a succeeded refund tombstone (#607): the
// admin refund producer reads reservation_id off the durable succeeded row to
// detect a double-refund conflict (admin_payments.go).
func (h *StripeRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func (h *StripeRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *StripeRefundHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	if _, _, err := subscriptions.RequireStripeSecretKey(h.Rails); err != nil {
		return Parked("stripe not configured: " + err.Error())
	}

	result, err := h.Stripe.CreateRefund(ctx, subscriptions.RefundParams{
		ChargeID: p.ProviderTarget,
		Amount:   p.AmountCents,
		Reason:   p.Reason,
		// The Stripe Idempotency-Key IS the intent's identity: any replay of
		// this intent (lease reclaim, verified-not-executed retry) returns the
		// original refund.
		IdempotencyKey: intent.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, stripeapi.ErrProviderReadOnly) {
			return Parked("stripe provider writes blocked (mode=readonly)")
		}
		var apiErr *subscriptions.StripeAPICallError
		if errors.As(err, &apiErr) {
			switch {
			case apiErr.StatusCode == 429:
				return Retryable("stripe rate limited: " + apiErr.Message)
			case apiErr.StatusCode >= 500:
				// Stripe answered 5xx: the refund may or may not exist.
				return Ambiguous("stripe server error: " + apiErr.Message)
			default:
				// 4xx: a clean refusal (already refunded, charge disputed...).
				return h.terminally(ctx, p, "stripe refund refused: "+apiErr.Message, map[string]any{
					"status_code": apiErr.StatusCode,
				})
			}
		}
		return Ambiguous("stripe refund outcome unknown: " + err.Error())
	}

	if strings.EqualFold(result.Status, "failed") {
		return h.terminally(ctx, p, "stripe refund failed: "+result.FailureReason, map[string]any{
			"provider_refund_id": result.ID,
			"failure_reason":     result.FailureReason,
		})
	}
	if err := h.finalize(ctx, p, result.ID); err != nil {
		return Ambiguous("refunded at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{"provider_refund_id": result.ID, "refund_status": result.Status})
}

func (h *StripeRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	result, found, err := h.Stripe.FindRefundByIdempotencyKey(ctx, p.ProviderTarget, intent.IdempotencyKey)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Retryable("refund not found at provider; verified not executed")
	}
	if strings.EqualFold(result.Status, "failed") {
		return h.terminally(ctx, p, "stripe refund failed post-create: "+result.FailureReason, map[string]any{
			"provider_refund_id": result.ID,
			"failure_reason":     result.FailureReason,
		})
	}
	if err := h.finalize(ctx, p, result.ID); err != nil {
		return Ambiguous("refund verified at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{"provider_refund_id": result.ID, "refund_status": result.Status, "verified_existing": true})
}

// RefundIntentFor derives the intent type, provider key and content-addressed
// idempotency key for refunding a payment, or an error when the rail has
// no ledger refund path. Shared by the admin producer so handler and producer
// can never disagree on identity.
func RefundIntentFor(payment *models.Payment, providerTarget string, amountCents moneyutil.Cents, reason string) (intentType, provider, idempotencyKey string, err error) {
	switch {
	case payment == nil:
		return "", "", "", errors.New("payment is required")
	case payment.Rail == models.RailStripe:
		return TypeStripeRefund, strings.ToLower(string(payment.Rail)),
			StripeRefundIntentIdempotencyKey(providerTarget, int64(amountCents), reason), nil
	default:
		return TypeNMIRefund, strings.ToLower(string(payment.Rail)),
			NMIRefundIdempotencyKey(providerTarget, int64(amountCents)), nil
	}
}
