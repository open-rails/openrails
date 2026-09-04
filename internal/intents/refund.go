package intents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/railresolve"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/productaccess"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
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
	TypeCCBillRefund = "ccbill_refund"
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
	AmountCents  moneyutil.Cents `json:"amount_cents"`
	Reason       string          `json:"reason,omitempty"`
	RevokeAccess bool            `json:"revoke_access"`
	// ProviderTarget is what the provider refunds against: the Stripe charge /
	// payment-intent id (ch_/pi_), the NMI transaction id of the original
	// payment, or — for CCBill — the CCBill subscriptionId (the
	// voidOrRefundTransaction action key).
	ProviderTarget string `json:"provider_target"`
	// ProviderTransactionID is the original provider transaction id, used only by
	// rails whose refund action needs BOTH a subscription id (ProviderTarget) and
	// the specific transaction — CCBill (voidOrRefundTransaction). Empty for
	// NMI/Stripe (ProviderTarget alone identifies the charge/transaction).
	ProviderTransactionID string `json:"provider_transaction_id,omitempty"`
}

// RefundIdempotencyKey binds one caller operation to its original payment.
// Merchant scope is enforced by the intent ledger's unique key.
func RefundIdempotencyKey(paymentID uuid.UUID, clientKey string) string {
	return fmt.Sprintf("refund:%s:%x", paymentID, sha256.Sum256([]byte(strings.TrimSpace(clientKey))))
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
		if db.IsNotFound(err) {
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
	// Keep the exact successful response even if the local transaction fails.
	// This is the only safe automatic recovery evidence for non-idempotent rails.
	if err := r.recordReceipt(ctx, p, providerRefundID); err != nil {
		return err
	}
	return r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := r.DB.NewWithPgxTx(tx)
		svc := payments.NewPaymentService(txDB, r.Clock)
		reservation, err := svc.GetByID(ctx, p.ReservationID)
		if err != nil {
			return err
		}
		if payments.PaymentStatusCompleted(reservation.Status) {
			return nil
		}
		if p.RevokeAccess {
			if err := entitlements.NewEntitlementService(txDB, r.Clock).EndActiveByPayment(ctx, p.OriginalPaymentID, models.EntitlementRevokeRefund); err != nil {
				return err
			}
			if _, err := productaccess.NewProductAccessGrantRepo(txDB).RevokeByPayment(ctx, p.OriginalPaymentID, r.now(), models.ProductAccessRevokeRefund); err != nil {
				return err
			}
		}
		metadata := reservation.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["admin_refund_status"] = "completed"
		metadata["provider_refund_id"] = providerRefundID
		_, err = svc.CompleteRefundReservation(ctx, p.ReservationID, providerRefundID, metadata)
		return err
	})
}

func (r refundReservations) now() time.Time {
	if r.Clock != nil {
		return r.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (r refundReservations) recordReceipt(ctx context.Context, p RefundPayload, providerRefundID string) error {
	if strings.TrimSpace(providerRefundID) == "" {
		return errors.New("successful refund response has no transaction reference")
	}
	_, err := r.DB.Qx(ctx).Exec(ctx, `UPDATE openrails.payments SET metadata = coalesce(metadata, '{}'::jsonb) || jsonb_build_object('provider_refund_id', $2::text) WHERE id=$1 AND status='pending'`, p.ReservationID, providerRefundID)
	return err
}

// recoverReceipt never guesses ownership from an amount or subscription counter.
// A lost response without an exact stored receipt requires operator verification.
func (r refundReservations) recoverReceipt(ctx context.Context, p RefundPayload) Outcome {
	reservation, err := r.payments().GetByID(ctx, p.ReservationID)
	if err != nil {
		return Ambiguous("load refund receipt: " + err.Error())
	}
	ref, _ := reservation.Metadata["provider_refund_id"].(string)
	if ref == "" {
		return Ambiguous("refund response has no exact operation receipt; operator must verify before completion or resend")
	}
	if err := r.finalize(ctx, p, ref); err != nil {
		return Ambiguous("refund receipt persisted but local finalization failed: " + err.Error())
	}
	return Succeeded(map[string]any{"provider_refund_id": ref, "recovered_receipt": true})
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
// intent per caller operation, never blind-retried — any outcome that
// MIGHT have moved money parks as unknown_needs_verify and the verifier
// resolves it by reading the transaction's refund actions off the Query API.
type NMIRefundHandler struct {
	refundReservations
	// Resolver arms the intent merchant's NMI client from the armed rail
	// state at drain time (#788).
	Resolver money.NMIClientResolver
	Policy   BackoffPolicy
}

func NewNMIRefundHandler(d *db.DB, resolver money.NMIClientResolver, clock clockwork.Clock) *NMIRefundHandler {
	return &NMIRefundHandler{
		refundReservations: refundReservations{DB: d, Clock: clock},
		Resolver:           resolver,
		Policy:             DefaultBackoff,
	}
}

func (h *NMIRefundHandler) Type() string                         { return TypeNMIRefund }
func (h *NMIRefundHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy keeps the payload on a succeeded refund tombstone (#607): replays use the linked reservation and immutable operation payload. The forensic evidence
// (provider_refund_id) is slimmed — the completed reservation row, not the
// intent, is the source of truth post-success.
func (h *NMIRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func (h *NMIRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *NMIRefundHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
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
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}

	if intent.Attempts > 1 {
		return h.recoverReceipt(ctx, p)
	}

	result, err := client.Refund(ctx, nmi.RefundParams{TransactionID: p.ProviderTarget, Amount: p.AmountCents})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return Parked("nmi provider writes blocked (mode=readonly)")
		}
		var pmErr *nmi.CustomerVaultError
		if errors.As(err, &pmErr) {
			// The gateway answered with a decline: the refund definitively did
			// not happen and re-sending the identical request cannot succeed.
			return h.terminally(ctx, p, "nmi refund declined: "+pmErr.Message, map[string]any{
				"response_code": pmErr.ResponseCode,
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

// Verify resumes known successful responses. NMI does not expose a caller
// operation key on refund actions: matching an amount can select an earlier
// partial refund, and absence in a read cannot prove a lost request never landed.
func (h *NMIRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	return h.recoverReceipt(ctx, p)
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
	Rails  railresolve.Source
	Stripe stripeRefundAPI
	Policy BackoffPolicy
}

func NewStripeRefundHandler(d *db.DB, cfg *config.Config, rails railresolve.Source, clock clockwork.Clock) *StripeRefundHandler {
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

// PrunePolicy keeps the payload on a succeeded refund tombstone (#607): replays use the linked reservation and immutable operation payload.
func (h *StripeRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func (h *StripeRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *StripeRefundHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	if _, _, err := subscriptions.RequireStripeSecretKey(ctx, h.Rails); err != nil {
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

// ============================================================================
// CCBill
// ============================================================================

// ccbillDenialMaxAttempts bounds clean retries of a CCBill DataLink DENIAL
// (ccbill.ErrDataLinkAuth: bare -7 or HTTP 401/403) before it is declared
// terminal. -7 is OVERLOADED — verified 2026-07-03 (safe fail-probe, no money
// moved): CCBill returns -7 BOTH for a wrong password AND for a refund/void of a
// too-old, non-refundable transaction with VALID creds. The operation did NOT
// execute either way, so a few clean retries cover a transient auth/IP flap; past
// that the denial is treated as PERMANENT (not permitted / not refundable /
// broken creds) and handed to an operator, never retried forever behind a
// misleading "auth" reason.
const ccbillDenialMaxAttempts = 3

// ccbillDenialExhausted reports whether a -7/auth denial has spent its bounded
// clean-retry budget and must go terminal. intent.Attempts is 1 on the first
// execution (the claim bumps it before Execute runs), so the budget is
// ccbillDenialMaxAttempts distinct send attempts.
func ccbillDenialExhausted(attempts int32) bool {
	return attempts >= ccbillDenialMaxAttempts
}

// CCBillRefundHandler executes refunds against CCBill via the DataLink SMS choke
// point (voidOrRefundTransaction). CCBill has NO request-level idempotency AND
// exposes no per-transaction refund read — only per-subscription refund/void
// COUNTERS — so effectively-once rests on the ledger (one intent per
// caller operation) and the money mover NEVER blind-resends: any
// outcome that MIGHT have moved money parks as unknown_needs_verify and the
// verifier resolves it against the counters (verify-not-decline, #674).
//
// WIRE PROVISIONAL — the refund request+response shape is unverified (cannot
// probe live: real money). See ccbill.DataLinkClient.RefundTransaction.
type CCBillRefundHandler struct {
	refundReservations
	Config *config.Config
	// Rails arms the intent merchant's DataLink client from the armed rail
	// state at drain time (#788).
	Rails railresolve.Source
	// DataLinkBaseURL overrides the DataLink endpoint (test seam).
	DataLinkBaseURL string
	Policy          BackoffPolicy
}

func NewCCBillRefundHandler(d *db.DB, cfg *config.Config, rails railresolve.Source, clock clockwork.Clock) *CCBillRefundHandler {
	return &CCBillRefundHandler{
		refundReservations: refundReservations{DB: d, Clock: clock},
		Config:             cfg,
		Rails:              rails,
		Policy:             DefaultBackoff,
	}
}

func (h *CCBillRefundHandler) Type() string                         { return TypeCCBillRefund }
func (h *CCBillRefundHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy keeps the payload on a succeeded refund tombstone (#607): the admin
// refund producer reads reservation_id off the durable succeeded row to detect a
// double-refund conflict (admin_payments.go).
func (h *CCBillRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func (h *CCBillRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *CCBillRefundHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	client, err := ccbillDataLinkForMerchant(ctx, h.Config, h.Rails, h.DataLinkBaseURL)
	if err != nil {
		return Parked("ccbill rail not armable (fail closed): " + err.Error())
	}
	if client.ReadOnly {
		return Parked("ccbill provider writes blocked (mode=readonly)")
	}
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	subscriptionID := strings.TrimSpace(p.ProviderTarget)
	transactionID := strings.TrimSpace(p.ProviderTransactionID)
	if transactionID == "" {
		// Never refund without the exact transaction id — CCBill would otherwise
		// pick the latest charge on the subscription.
		return Terminal("ccbill refund requires the original transaction id (provider_transaction_id)")
	}

	// Only a definitive -7 refusal permits another send. An expired lease or
	// unknown response must recover an exact receipt, never infer from counters.
	if intent.Attempts > 1 && (intent.LastFailureReason == nil || !strings.HasPrefix(*intent.LastFailureReason, "ccbill refund denied (-7):")) {
		return h.recoverReceipt(ctx, p)
	}

	res, err := client.RefundTransaction(ctx, subscriptionID, transactionID, p.AmountCents)
	if err != nil {
		switch {
		case errors.Is(err, ccbill.ErrProviderReadOnly):
			return Parked("ccbill provider writes blocked (mode=readonly)")
		case errors.Is(err, ccbill.ErrDataLinkAuth):
			// -7 is CCBill's OVERLOADED denial (auth/IP OR operation-not-permitted,
			// e.g. a too-old / non-refundable transaction). It did NOT execute
			// (safe — a counter re-read confirms no money moved), but it may be
			// PERMANENT: bounded clean-retry for a transient auth/IP flap, then
			// terminal for the operator — never infinite retry behind a misleading
			// "auth" reason. Terminal releases the reservation.
			if ccbillDenialExhausted(intent.Attempts) {
				return h.terminally(ctx, p,
					"ccbill refund denied (-7): provider refused after bounded retries — not permitted / not refundable / auth — needs operator attention",
					map[string]any{"denial_code": "-7", "attempts": intent.Attempts})
			}
			return Retryable("ccbill refund denied (-7): provider refused (auth/IP, or not permitted / not refundable); bounded retry")
		default:
			// Definite reject (ErrRefundRejected) or transport/unparseable
			// ambiguity — the refund MAY have moved money. Verifier reads before
			// any decline (#674 verify-not-decline).
			return Ambiguous("ccbill refund outcome unknown: " + err.Error())
		}
	}
	if err := h.finalize(ctx, p, ccbillRefundProviderRef(subscriptionID, transactionID, p.ReservationID)); err != nil {
		return Ambiguous("refunded at provider, but local finalize failed: " + err.Error())
	}
	return Succeeded(map[string]any{
		"results":              res.Results,
		"action":               res.Action,
		"rail_subscription_id": subscriptionID,
		"transaction_id":       transactionID,
	})
}

// Verify completes a captured response; subscription counters cannot identify a
// particular refund operation, including a second equal-size partial refund.
func (h *CCBillRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	return h.recoverReceipt(ctx, p)
}

// ccbillRefundProviderRef is the synthetic provider reference recorded on the
// completed reservation. CCBill answers a refund with a bare results code, NOT a
// refund id, so the stable reference is the (subscription, transaction) pair.
func ccbillRefundProviderRef(subscriptionID, transactionID string, reservationID uuid.UUID) string {
	return "ccbill_refund:" + subscriptionID + ":" + transactionID + ":" + reservationID.String()
}

// RefundIntentFor selects the rail while keeping one caller operation identity.
func RefundIntentFor(payment *models.Payment, clientKey string) (intentType, provider, idempotencyKey string, err error) {
	if payment == nil {
		return "", "", "", errors.New("payment is required")
	}
	provider = strings.ToLower(string(payment.Rail))
	switch payment.Rail {
	case models.RailStripe:
		intentType = TypeStripeRefund
	case models.RailCCBill:
		intentType = TypeCCBillRefund
	default:
		intentType = TypeNMIRefund
	}
	return intentType, provider, RefundIdempotencyKey(payment.ID, clientKey), nil
}
