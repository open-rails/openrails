package intents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
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
	// exactly from the payment-currency native amount at the admin boundary.
	AmountCents  moneyutil.Cents `json:"amount_cents"`
	Currency     string          `json:"currency"`
	Reason       string          `json:"reason,omitempty"`
	RevokeAccess bool            `json:"revoke_access"`
	// ProviderTarget is what the provider refunds against: the Stripe charge /
	// payment-intent id (ch_/pi_), the NMI transaction id of the original
	// payment. Existing CCBill payloads retain their subscriptionId for review.
	ProviderTarget string `json:"provider_target"`
	// ProviderTransactionID preserves the requested charge in existing CCBill
	// payloads. Its presence never proved the provider honored exact targeting.
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

// DecodeRefundPayload validates the accepted amount/currency and target for execution and portable replay.
func DecodeRefundPayload(intent gen.OpenrailsRailIntent) (RefundPayload, error) {
	var p RefundPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("refund intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode refund payload: %w", err)
	}
	if p.OriginalPaymentID == uuid.Nil || p.ReservationID == uuid.Nil || strings.TrimSpace(p.ProviderTarget) == "" || p.AmountCents <= 0 {
		return p, errors.New("refund payload is incomplete (original payment, reservation, provider target and amount are required)")
	}
	if _, ok := moneyutil.LookupCurrency(p.Currency); !ok || p.Currency == "" || p.Currency != strings.ToUpper(strings.TrimSpace(p.Currency)) {
		return p, errors.New("refund payload requires its canonical payment currency")
	}
	return p, nil
}

// checkRelevance: a refund intent applies while its local reservation is
// still open (pending). A completed reservation means the refund already
// finalized; a released/failed or deleted one means it was abandoned.
func (r refundReservations) checkRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := DecodeRefundPayload(intent)
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
	// Keep the exact successful response even if the local transaction fails or
	// the caller is gone: it is the only safe automatic recovery evidence for
	// non-idempotent rails.
	receiptCtx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	if err := r.recordReceipt(receiptCtx, p, providerRefundID); err != nil {
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
			if err := r.revokeMembershipAccess(ctx, txDB, p.OriginalPaymentID); err != nil {
				return err
			}
			if err := entitlements.NewEntitlementService(txDB, r.Clock).EndActiveByPayment(ctx, p.OriginalPaymentID, models.EntitlementRevokeRefund); err != nil {
				return err
			}
			if _, err := productaccess.NewProductAccessGrantRepo(txDB).RevokeByPayment(ctx, p.OriginalPaymentID, r.now(), models.ProductAccessRevokeRefund); err != nil {
				return err
			}
		}
		// SEC-33: a provider notice recorded this refund first; keep that row and
		// release the reservation instead of colliding on the transaction id.
		original, err := svc.GetByID(ctx, p.OriginalPaymentID)
		if err != nil {
			return err
		}
		lookup := ctx
		if original.PspID != nil {
			lookup = db.WithPSPID(ctx, *original.PspID)
		}
		if existing, err := svc.GetByPSPTransactionID(lookup, original.Rail, providerRefundID); err == nil && existing.ID != reservation.ID {
			if existing.RefundedPaymentID == nil || *existing.RefundedPaymentID != original.ID {
				return fmt.Errorf("provider refund %s is recorded against another payment", providerRefundID)
			}
			return svc.MarkFailed(ctx, p.ReservationID)
		} else if err != nil && !db.IsNotFound(err) {
			return err
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

// revokeMembershipAccess ends the access a refunded membership payment
// bought. An engine membership is also cancelled: the engine would otherwise
// renew it and grant access again; so is a provider-billed NMI membership,
// whose schedule is deleted.
func (r refundReservations) revokeMembershipAccess(ctx context.Context, d *db.DB, paymentID uuid.UUID) error {
	original, err := payments.NewPaymentService(d, r.Clock).GetByID(ctx, paymentID)
	if err != nil {
		return err
	}
	if original.SubscriptionID == nil {
		return nil
	}
	sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, *original.SubscriptionID)
	if err != nil {
		return err
	}
	if sub.CollectionPolicy == models.CollectionPolicyEngine && (sub.Status == models.StatusActive || sub.Status == models.StatusPastDue) {
		reason := "payment refunded with access revoked"
		lifecycle := subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, payments.NewPaymentService(d, r.Clock), r.Clock)
		_, err := lifecycle.CancelMembershipTx(ctx, d, &subscriptions.CancelMembershipParams{SubscriptionID: &sub.ID, CancelType: models.CancelTypeMerchant, CancelFeedback: &reason, RevokeAccess: true})
		return err
	}
	if subscriptions.NeedsProviderScheduleDelete(sub) && sub.Status.Live() {
		// A provider-billed membership whose payment is refunded with access
		// revoked ends now: otherwise the provider bills it again and the
		// mirror re-grants access. Its schedule delete rides the same durable
		// nmi_delete operation as a cancel.
		now := r.now()
		cancelType := models.CancelTypeMerchant
		reason := "payment refunded with access revoked"
		sub.Status, sub.CancelledAt, sub.CancelType, sub.CancelFeedback = models.StatusCancelled, &now, &cancelType, &reason
		sub.DeletionScheduledAt = &now
		sub.ClearRetrySchedule()
		sub.MarkLifecycleDecision("refund_revoke")
		if err := subscriptions.NewSubscriptionRepo(d).UpdateAt(ctx, sub, now); err != nil {
			return err
		}
		if err := NewNMIDeleteScheduler(d, nil, OriginAdmin, "refund with access revoked").ScheduleNMIDelete(ctx, sub.CustomerID.String(), sub.ID, now); err != nil {
			return err
		}
	}
	return entitlements.NewEntitlementService(d, r.Clock).RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, r.now(), models.EntitlementRevokeRefund, models.EntitlementSourceSubscription, models.EntitlementSourceGrace)
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
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return r.DB.Gen(ctx).RecordRefundProviderReceipt(ctx, gen.RecordRefundProviderReceiptParams{
		MerchantID: mid.UUID(), ReservationID: p.ReservationID, ProviderRefundID: providerRefundID,
	})
}

// recoverReceipt never guesses ownership from an amount or subscription counter.
// A lost response without an exact stored receipt requires operator verification.
func (r refundReservations) recoverReceipt(ctx context.Context, intent gen.OpenrailsRailIntent, p RefundPayload) Outcome {
	ref, err := r.knownReceipt(ctx, intent, p)
	if err != nil {
		return Ambiguous("load refund receipt: " + err.Error())
	}
	if ref == "" {
		return Ambiguous("refund response has no exact operation receipt; operator must review provider records, and unavailable evidence keeps the refund unresolved")
	}
	return r.settle(ctx, p, ref, map[string]any{"recovered_receipt": true})
}

// settle finalizes a refund the provider confirmed with the exact receipt ref.
// A failed local finalize keeps ref on the ambiguous outcome's evidence, so the
// unknown mark retains it even when the receipt write itself did not land.
func (r refundReservations) settle(ctx context.Context, p RefundPayload, ref string, evidence map[string]any) Outcome {
	if err := r.finalize(ctx, p, ref); err != nil {
		return AmbiguousWithEvidence("provider refund "+ref+" succeeded, but local finalization failed: "+err.Error(), map[string]any{"provider_refund_id": ref})
	}
	evidence["provider_refund_id"] = ref
	return Succeeded(evidence)
}

// knownReceipt returns the exact provider refund id captured on the
// reservation or, when that local write failed, on the operation evidence.
func (r refundReservations) knownReceipt(ctx context.Context, intent gen.OpenrailsRailIntent, p RefundPayload) (string, error) {
	reservation, err := r.payments().GetByID(ctx, p.ReservationID)
	if err != nil {
		return "", err
	}
	if ref, _ := reservation.Metadata["provider_refund_id"].(string); ref != "" {
		return ref, nil
	}
	return EvidenceString(intent, "provider_refund_id"), nil
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
// MIGHT have moved money parks as unknown_needs_verify. Recovery requires a
// captured operation receipt or explicit operator verification.
type NMIRefundHandler struct {
	refundReservations
	// Resolver arms the intent merchant's NMI client from the armed rail
	// state at drain time (#788).
	Resolver NMIClientResolver
	Policy   BackoffPolicy
}

func NewNMIRefundHandler(d *db.DB, resolver NMIClientResolver, clock clockwork.Clock) *NMIRefundHandler {
	return &NMIRefundHandler{
		refundReservations: refundReservations{DB: d, Clock: clock},
		Resolver:           resolver,
		Policy:             DefaultBackoff,
	}
}

func (h *NMIRefundHandler) Type() string                         { return TypeNMIRefund }
func (h *NMIRefundHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy retains the operation payload for replay. The forensic evidence
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
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}

	if intent.Attempts > 1 {
		return h.recoverReceipt(ctx, intent, p)
	}

	result, err := client.Refund(ctx, nmi.RefundParams{TransactionID: p.ProviderTarget, Amount: p.AmountCents, Currency: p.Currency})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return Parked("nmi provider writes blocked (mode=readonly)")
		}
		if errors.Is(err, providerposture.ErrDisarmed) {
			// The posture gate refused before any bytes were sent.
			return Parked("nmi refund held by sandbox posture gate: " + err.Error())
		}
		if nmi.RequiresVerification(err) {
			// Processor communication/duplicate responses do not prove the
			// refund did not land; releasing the reservation could refund twice.
			return Ambiguous("nmi refund outcome unknown: " + err.Error())
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

	return h.settle(ctx, p, result.TransactionID, map[string]any{})
}

// Verify resumes known successful responses. NMI does not expose a caller
// operation key on refund actions: matching an amount can select an earlier
// partial refund, and absence in a read cannot prove a lost request never landed.
func (h *NMIRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	return h.recoverReceipt(ctx, intent, p)
}

// Resolve accepts an exact refund transaction the provider confirms is an
// approved refund of the reserved amount and currency on the original sale's
// vault. NMI read absence cannot establish nonexecution of a submitted refund,
// so that resolution is refused. It never re-sends the refund.
func (h *NMIRefundHandler) Resolve(ctx context.Context, intent gen.OpenrailsRailIntent, resolution Resolution) (Outcome, error) {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Outcome{}, err
	}
	if resolution.Step != "" {
		return Outcome{}, fmt.Errorf("%w: a refund has no steps", ErrResolutionInvalid)
	}
	ref, err := h.knownReceipt(ctx, intent, p)
	if err != nil {
		return Outcome{}, err
	}
	if ref != "" {
		return Outcome{}, RejectResolution("operation already holds refund receipt %s; its verifier completes it", ref)
	}
	if resolution.NotExecuted {
		return Outcome{}, RejectResolution("NMI does not provide definitive nonexecution proof for a possibly submitted refund; the reservation must remain held")
	}
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil || !ok || client == nil {
		return Outcome{}, fmt.Errorf("nmi rail is not armed for provider %q: %v", intent.Rail, err)
	}
	if err := client.ConfirmRefund(ctx, p.ProviderTarget, resolution.ProviderReference, p.AmountCents, p.Currency); err != nil {
		return Outcome{}, RejectResolution("%v", err)
	}
	if err := h.finalize(ctx, p, resolution.ProviderReference); err != nil {
		return AmbiguousWithEvidence("confirmed refund retained but local finalization failed: "+err.Error(), map[string]any{"provider_refund_id": resolution.ProviderReference}), nil
	}
	return Succeeded(map[string]any{"provider_refund_id": resolution.ProviderReference}), nil
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

func NewStripeRefundHandler(d *db.DB, cfg *config.Config, rails railresolve.Source, clock clockwork.Clock, clients *stripeapi.Factory) *StripeRefundHandler {
	return &StripeRefundHandler{
		refundReservations: refundReservations{DB: d, Clock: clock},
		Config:             cfg,
		Rails:              rails,
		Stripe:             &subscriptions.StripeRefundService{StripeClients: clients, Config: cfg, Rails: rails},
		Policy:             DefaultBackoff,
	}
}

func (h *StripeRefundHandler) Type() string                         { return TypeStripeRefund }
func (h *StripeRefundHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy retains the immutable operation payload for replay.
func (h *StripeRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func (h *StripeRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *StripeRefundHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
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
		if errors.Is(err, providerposture.ErrDisarmed) {
			return Parked("stripe refund held by sandbox posture gate: " + err.Error())
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

	if strings.EqualFold(result.Status, "failed") || strings.EqualFold(result.Status, "canceled") {
		return h.terminally(ctx, p, "stripe refund failed: "+result.FailureReason, map[string]any{
			"provider_refund_id": result.ID,
			"failure_reason":     result.FailureReason,
		})
	}
	if !strings.EqualFold(result.Status, "succeeded") {
		return Ambiguous("stripe refund has not succeeded: " + result.Status)
	}
	return h.settle(ctx, p, result.ID, map[string]any{"refund_status": result.Status})
}

func (h *StripeRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	// A captured successful receipt is exact: settle from it instead of
	// depending on provider list visibility, whose miss would requeue the create
	// after the provider's idempotency window may have lapsed. An unreadable
	// reservation proves nothing either way — the provider read below decides.
	if ref, rerr := h.knownReceipt(ctx, intent, p); rerr == nil && ref != "" {
		return h.settle(ctx, p, ref, map[string]any{"recovered_receipt": true})
	}
	result, found, err := h.Stripe.FindRefundByIdempotencyKey(ctx, p.ProviderTarget, intent.IdempotencyKey)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Retryable("refund not visible; retry through provider-enforced idempotency key")
	}
	if strings.EqualFold(result.Status, "failed") || strings.EqualFold(result.Status, "canceled") {
		return h.terminally(ctx, p, "stripe refund failed post-create: "+result.FailureReason, map[string]any{
			"provider_refund_id": result.ID,
			"failure_reason":     result.FailureReason,
		})
	}
	if !strings.EqualFold(result.Status, "succeeded") {
		return Ambiguous("stripe refund has not succeeded: " + result.Status)
	}
	return h.settle(ctx, p, result.ID, map[string]any{"refund_status": result.Status, "verified_existing": true})
}

// ============================================================================
// CCBill
// ============================================================================

// CCBillRefundHandler retains unresolved reservations created by earlier builds.
// The provider's subscription-level API cannot establish the requested charge,
// amount, and lifecycle outcome. No execution, resend, or synthetic-receipt
// finalization is safe until an operator supplies independent provider evidence.
type CCBillRefundHandler struct {
	refundReservations
	Policy BackoffPolicy
}

func NewCCBillRefundHandler(d *db.DB, clock clockwork.Clock) *CCBillRefundHandler {
	return &CCBillRefundHandler{refundReservations: refundReservations{DB: d, Clock: clock}, Policy: DefaultBackoff}
}

func (h *CCBillRefundHandler) Type() string                                  { return TypeCCBillRefund }
func (h *CCBillRefundHandler) Backoff(attempts int32) time.Duration          { return h.Policy.Delay(attempts) }
func (h *CCBillRefundHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, true }
func (h *CCBillRefundHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}
func (h *CCBillRefundHandler) Execute(context.Context, gen.OpenrailsRailIntent) Outcome {
	return Ambiguous("automatic CCBill refund disabled: retain reservation and receipt; operator must verify the actual transaction, amount, and subscription state")
}
func (h *CCBillRefundHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	return h.Execute(ctx, intent)
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
		return "", "", "", ccbill.ErrRefundUnsupported
	default:
		intentType = TypeNMIRefund
	}
	return intentType, provider, RefundIdempotencyKey(payment.ID, clientKey), nil
}
