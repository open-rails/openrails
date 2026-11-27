package intents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/railresolve"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/modules/entitlements"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Refund intent types. The producer reserves a negative pending payment row;
// the handler's finalize completes it with the provider refund id on success
// (so the async drain repairs a producer that died mid-call) or releases it
// (status=failed) on a terminal refusal.
const (
	TypeNMIRefund           = "nmi_refund"
	TypeStripeRefund        = "stripe_refund"
	RefundUnresolvedFinding = "life.refund.unresolved"
)

// RefundPayload is the stored payload for refund intents.
type RefundPayload struct {
	OriginalPaymentID uuid.UUID `json:"original_payment_id"`
	// ReservationID is the local negative pending payment row the finalize
	// completes (provider id recorded) or releases (terminal refusal).
	ReservationID uuid.UUID `json:"reservation_id"`
	// AmountCents is provider minor units (typed Cents), converted exactly
	// from the payment-currency native amount at the admin boundary.
	AmountCents moneyutil.Cents `json:"amount_cents"`
	Currency    string          `json:"currency"`
	Reason      string          `json:"reason,omitempty"`
	// RefundReason is why OpenRails refunds; it names the provider's reason.
	RefundReason billing.RefundReason `json:"refund_reason,omitempty"`
	RevokeAccess bool                 `json:"revoke_access"`
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
func DecodeRefundPayload(intent gen.BillingProviderIntent) (RefundPayload, error) {
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
	if err := moneyutil.RequireFiatCurrency(p.Currency); err != nil {
		return p, err
	}
	return p, nil
}

// checkRelevance: a refund intent applies while its local reservation is
// still open (pending). A completed reservation means the refund already
// finalized; a released/failed or deleted one means it was abandoned.
func (r refundReservations) checkRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
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
	case payments.PaymentStatusSucceededValue, "":
		return SupersededBy("refund reservation already completed"), nil
	default:
		return SupersededBy(fmt.Sprintf("refund reservation released (status=%s)", reservation.Status)), nil
	}
}

// finalize completes the reservation with the provider refund id. Idempotent:
// an already-completed reservation is left alone. Metadata is carried over
// from the reservation (CompleteRefundReservation replaces it wholesale) with
// the completion stamped in. The intent's subject (the staff member who
// asked) is recorded as the actor on any access it revokes.
func (r refundReservations) finalize(ctx context.Context, intent gen.BillingProviderIntent, p RefundPayload, providerRefundID string) error {
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
		if _, err := txDB.Gen(ctx).ResolveStandingFinding(ctx, gen.ResolveStandingFindingParams{MerchantID: intent.MerchantID, FindingType: RefundUnresolvedFinding, SubjectKey: intent.ID.String()}); err != nil {
			return err
		}
		svc := payments.NewPaymentService(txDB, r.Clock)
		reservation, err := svc.GetByID(ctx, p.ReservationID)
		if err != nil {
			return err
		}
		if payments.PaymentStatusSucceeded(reservation.Status) {
			return nil
		}
		if p.RevokeAccess {
			var actor string
			if intent.Subject != nil {
				actor = *intent.Subject
			}
			if err := entitlements.NewEntitlementService(txDB, r.Clock).RevokeRefundedPaymentAccess(ctx, p.OriginalPaymentID, actor); err != nil {
				return err
			}
		}
		// A provider notice recorded this refund first; keep that row and
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
		if _, err := svc.CompleteRefundReservation(ctx, p.ReservationID, providerRefundID, metadata); err != nil {
			return err
		}
		if !p.RevokeAccess {
			return nil
		}
		return subscriptions.EndRefundedMembership(ctx, txDB, r.Clock, original, p.Reason)
	})
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
func (r refundReservations) recoverReceipt(ctx context.Context, intent gen.BillingProviderIntent, p RefundPayload) Outcome {
	ref, err := r.knownReceipt(ctx, intent, p)
	if err != nil {
		return Ambiguous("load refund receipt: " + err.Error())
	}
	if ref == "" {
		reason := "refund response has no exact operation receipt; operator must review provider records, and unavailable evidence keeps the refund unresolved"
		if intent.LastFailureReason != nil && *intent.LastFailureReason != "" {
			reason = *intent.LastFailureReason
		}
		evidence := struct {
			OperationID string          `json:"operation_id"`
			PaymentID   string          `json:"payment_id"`
			Amount      moneyutil.Cents `json:"amount"`
			Currency    string          `json:"currency"`
			Reason      string          `json:"reason"`
		}{intent.ID.String(), billing.PaymentID(p.OriginalPaymentID).String(), p.AmountCents, p.Currency, reason}
		raw, _ := json.Marshal(evidence)
		action := fmt.Sprintf("NMI cannot confirm the refund of %s on payment %s (%s). Nothing is resent, and the amount stays held so it cannot be refunded twice. Find this sale's refunds at NMI: if this refund went through, run `openrails intents resolve --intent %s --receipt <refund transaction id>`; if NMI shows no refund of the sale that OpenRails has not recorded, run `openrails intents resolve --intent %s --not-executed` with --actor and --reason.", moneyutil.FormatRailMinor(p.AmountCents, p.Currency), evidence.PaymentID, reason, intent.ID, intent.ID)
		wctx, cancel := LedgerWriteContext(ctx)
		defer cancel()
		if _, err := r.DB.Gen(wctx).UpsertReconciliationFinding(wctx, gen.UpsertReconciliationFindingParams{MerchantID: intent.MerchantID, FindingType: RefundUnresolvedFinding, SubjectKey: intent.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: raw}); err != nil {
			r.DB.Log.ErrorContext(ctx, "nmi refund: could not record the unresolved-refund finding", logx.Err(err), "operation_id", intent.ID)
		}
		return Ambiguous(reason)
	}
	return r.settle(ctx, intent, p, ref, map[string]any{"recovered_receipt": true})
}

// settle finalizes a refund the provider confirmed with the exact receipt ref.
// A failed local finalize keeps ref on the ambiguous outcome's evidence, so the
// unknown mark retains it even when the receipt write itself did not land.
func (r refundReservations) settle(ctx context.Context, intent gen.BillingProviderIntent, p RefundPayload, ref string, evidence map[string]any) Outcome {
	if err := r.finalize(ctx, intent, p, ref); err != nil {
		return AmbiguousWithEvidence("provider refund "+ref+" succeeded, but local finalization failed: "+err.Error(), map[string]any{"provider_refund_id": ref})
	}
	evidence["provider_refund_id"] = ref
	return Succeeded(evidence)
}

// knownReceipt returns the exact provider refund id captured on the
// reservation or, when that local write failed, on the operation evidence.
func (r refundReservations) knownReceipt(ctx context.Context, intent gen.BillingProviderIntent, p RefundPayload) (string, error) {
	reservation, err := r.payments().GetByID(ctx, p.ReservationID)
	if err != nil {
		return "", err
	}
	if ref, _ := reservation.Metadata["provider_refund_id"].(string); ref != "" {
		return ref, nil
	}
	return EvidenceString(intent, "provider_refund_id"), nil
}

// terminally releases the reservation and reports the terminal outcome; a
// failed release keeps the outcome ambiguous so the lease/verifier retries
// the release rather than stranding an open reservation on a dead intent.
func (r refundReservations) terminally(ctx context.Context, intent gen.BillingProviderIntent, p RefundPayload, reason string, evidence map[string]any) Outcome {
	if err := r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := r.DB.NewWithPgxTx(tx)
		if err := payments.NewPaymentService(txDB, r.Clock).MarkFailed(ctx, p.ReservationID); err != nil {
			return err
		}
		_, err := txDB.Gen(ctx).ResolveStandingFinding(ctx, gen.ResolveStandingFindingParams{MerchantID: intent.MerchantID, FindingType: RefundUnresolvedFinding, SubjectKey: intent.ID.String()})
		return err
	}); err != nil {
		return Ambiguous(reason + " (and reservation release failed: " + err.Error() + ")")
	}
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidence["destructive_no_effect"] = true // canonical terminal decline/release, never an unknown provider outcome
	return TerminalWithEvidence(reason, evidence)
}

// NMIRefundHandler executes refunds against NMI-backed rails. NMI has no
// request-level idempotency, so effectively-once rests on the ledger: one
// intent per caller operation, never blind-retried — any outcome that
// MIGHT have moved money parks as unknown_needs_verify. Recovery requires a
// captured operation receipt or explicit operator verification.
type NMIRefundHandler struct {
	refundReservations
	// Resolver arms the intent merchant's NMI client from the armed rail
	// state at drain time.
	Resolver railresolve.NMIClientResolver
	Policy   BackoffPolicy
}

func NewNMIRefundHandler(d *db.DB, resolver railresolve.NMIClientResolver, clock clockwork.Clock) *NMIRefundHandler {
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

func (h *NMIRefundHandler) CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *NMIRefundHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil {
		return parkedBeforeEffect("nmi rail not armable (fail closed): " + err.Error())
	}
	if !ok || client == nil {
		return parkedBeforeEffect(fmt.Sprintf("nmi rail is not armed for provider %q", intent.Rail))
	}
	if client.ReadOnly {
		return parkedBeforeEffect("nmi client is read-only (readonly write posture)")
	}

	if intent.Attempts > 1 {
		return h.recoverReceipt(ctx, intent, p)
	}

	ctx, err = startProtectedEffect(ctx, h.DB, intent, h.Clock)
	if err != nil {
		return parkedBeforeEffect(err.Error())
	}
	result, err := client.Refund(ctx, nmi.RefundParams{TransactionID: p.ProviderTarget, Amount: p.AmountCents, Currency: p.Currency})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return parkedBeforeEffect("nmi provider writes blocked (readonly write posture)")
		}
		if errors.Is(err, providerposture.ErrDisarmed) {
			// The posture gate refused before any bytes were sent.
			return parkedBeforeEffect("nmi refund held by sandbox posture gate: " + err.Error())
		}
		var refusal *nmi.V5Error
		if errors.As(err, &refusal) && refusal.Code != "" && refusal.Status >= 400 && refusal.Status < 500 {
			if refusal.Status == 429 {
				return h.terminally(ctx, intent, p, "nmi refund refused: NMI rate limited the refund and nothing was refunded, so it can be requested again", map[string]any{"status_code": refusal.Status})
			}
			return h.terminally(ctx, intent, p, "nmi refund refused: "+err.Error(), map[string]any{"status_code": refusal.Status})
		}
		var rejection *nmi.CustomerVaultError
		if errors.Is(err, nmi.ErrSaleRefused) && errors.As(err, &rejection) {
			return h.terminally(ctx, intent, p, fmt.Sprintf("nmi refund refused by the gateway without processing it (response code %d)", rejection.ResponseCode), map[string]any{"response_code": rejection.ResponseCode})
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
			return h.terminally(ctx, intent, p, "nmi refund declined: "+pmErr.Message, map[string]any{
				"response_code": pmErr.ResponseCode,
			})
		}
		// Transport-level failure: the refund MAY have been processed.
		return Ambiguous("nmi refund outcome unknown: " + err.Error())
	}

	return h.settle(ctx, intent, p, result.TransactionID, map[string]any{})
}

// Verify resumes known successful responses. NMI does not expose a caller
// operation key on refund actions: matching an amount can select an earlier
// partial refund, and absence in a read cannot prove a lost request never landed.
func (h *NMIRefundHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	return h.recoverReceipt(ctx, intent, p)
}

// Resolve accepts a confirmed receipt of the refund on the original sale, or
// an operator-attested release when NMI shows no unrecorded refunds of that sale.
// It never re-sends the refund.
func (h *NMIRefundHandler) Resolve(ctx context.Context, intent gen.BillingProviderIntent, resolution Resolution) (Outcome, error) {
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
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil || !ok || client == nil {
		return Outcome{}, fmt.Errorf("nmi rail is not armed for provider %q: %v", intent.Rail, err)
	}
	if resolution.NotExecuted {
		reservation, err := h.payments().GetByID(ctx, p.ReservationID)
		if err != nil {
			return Outcome{}, RejectResolution("load refund reservation: %v", err)
		}
		original, err := h.payments().GetByID(ctx, p.OriginalPaymentID)
		if err != nil {
			return Outcome{}, RejectResolution("load original payment: %v", err)
		}
		ids, err := client.ReadRefundTransactions(ctx, p.ProviderTarget, reservation.CreatedAt.Add(-time.Hour).Truncate(time.Second))
		if err != nil {
			return Outcome{}, RejectResolution("read NMI refunds: %v", err)
		}
		lookup := ctx
		if original.PspID != nil {
			lookup = db.WithPSPID(ctx, *original.PspID)
		}
		for _, id := range ids {
			if _, err := h.payments().GetByPSPTransactionID(lookup, original.Rail, id); db.IsNotFound(err) {
				return Outcome{}, RejectResolution("NMI shows refund %s of this sale that OpenRails has not recorded; if it is this refund, resolve with --receipt %s", id, id)
			} else if err != nil {
				return Outcome{}, RejectResolution("look up NMI refund %s: %v", id, err)
			}
		}
		return h.terminally(ctx, intent, p, "operator attested release; NMI shows no unrecorded refund of the sale", map[string]any{"operator_resolution": OperatorResolutionRecord(ctx)}), nil
	}
	if err := client.ConfirmRefund(ctx, p.ProviderTarget, resolution.ProviderReference, p.AmountCents, p.Currency); err != nil {
		return Outcome{}, RejectResolution("%v", err)
	}
	if err := h.finalize(ctx, intent, p, resolution.ProviderReference); err != nil {
		return AmbiguousWithEvidence("confirmed refund retained but local finalization failed: "+err.Error(), map[string]any{"provider_refund_id": resolution.ProviderReference}), nil
	}
	return Succeeded(map[string]any{"provider_refund_id": resolution.ProviderReference}), nil
}

// StripeRefundHandler executes refunds against Stripe. Effectively-once is
// double-walled: the ledger dedupes intents AND every create carries the
// intent's idempotency_key as the Stripe Idempotency-Key header, so even a
// raced duplicate POST replays the original refund instead of minting a
// second one. The verifier re-finds refunds via reads (GET /v1/refunds,
// matched on the metadata mirror of the key).
type StripeRefundHandler struct {
	refundReservations
	Config *billing.Config
	Rails  railresolve.Source
	Stripe *subscriptions.StripeRefundService
	Policy BackoffPolicy
}

func NewStripeRefundHandler(d *db.DB, cfg *billing.Config, rails railresolve.Source, clock clockwork.Clock, clients *stripeapi.Factory) *StripeRefundHandler {
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

func (h *StripeRefundHandler) CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
	return h.checkRelevance(ctx, intent)
}

func (h *StripeRefundHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	if _, _, err := subscriptions.RequireStripeSecretKey(ctx, h.Rails); err != nil {
		return parkedBeforeEffect("stripe not configured: " + err.Error())
	}

	ctx, err = startProtectedEffect(ctx, h.DB, intent, h.Clock)
	if err != nil {
		return parkedBeforeEffect(err.Error())
	}
	result, err := h.Stripe.CreateRefund(ctx, subscriptions.RefundParams{
		ChargeID: p.ProviderTarget,
		Amount:   p.AmountCents,
		Reason:   stripeRefundReason(p),
		// The Stripe Idempotency-Key IS the intent's identity: any replay of
		// this intent (lease reclaim, verified-not-executed retry) returns the
		// original refund.
		IdempotencyKey: intent.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, stripeapi.ErrProviderReadOnly) {
			return parkedBeforeEffect("stripe provider writes blocked (readonly write posture)")
		}
		if errors.Is(err, providerposture.ErrDisarmed) {
			return parkedBeforeEffect("stripe refund held by sandbox posture gate: " + err.Error())
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
				return h.terminally(ctx, intent, p, "stripe refund refused: "+apiErr.Message, map[string]any{
					"status_code": apiErr.StatusCode,
				})
			}
		}
		return Ambiguous("stripe refund outcome unknown: " + err.Error())
	}

	if strings.EqualFold(result.Status, "failed") || strings.EqualFold(result.Status, "canceled") {
		return h.terminally(ctx, intent, p, "stripe refund failed: "+result.FailureReason, map[string]any{
			"provider_refund_id": result.ID,
			"failure_reason":     result.FailureReason,
		})
	}
	if !strings.EqualFold(result.Status, "succeeded") {
		return Ambiguous("stripe refund has not succeeded: " + result.Status)
	}
	return h.settle(ctx, intent, p, result.ID, map[string]any{"refund_status": result.Status})
}

func (h *StripeRefundHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	p, err := DecodeRefundPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	// A captured successful receipt is exact: settle from it instead of
	// depending on provider list visibility, whose miss would requeue the create
	// after the provider's idempotency window may have lapsed. An unreadable
	// reservation proves nothing either way — the provider read below decides.
	if ref, rerr := h.knownReceipt(ctx, intent, p); rerr == nil && ref != "" {
		return h.settle(ctx, intent, p, ref, map[string]any{"recovered_receipt": true})
	}
	result, found, err := h.Stripe.FindRefundByIdempotencyKey(ctx, p.ProviderTarget, intent.IdempotencyKey)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Retryable("refund not visible; retry through provider-enforced idempotency key")
	}
	if strings.EqualFold(result.Status, "failed") || strings.EqualFold(result.Status, "canceled") {
		return h.terminally(ctx, intent, p, "stripe refund failed post-create: "+result.FailureReason, map[string]any{
			"provider_refund_id": result.ID,
			"failure_reason":     result.FailureReason,
		})
	}
	if !strings.EqualFold(result.Status, "succeeded") {
		return Ambiguous("stripe refund has not succeeded: " + result.Status)
	}
	return h.settle(ctx, intent, p, result.ID, map[string]any{"refund_status": result.Status, "verified_existing": true})
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

// stripeRefundReason is the reason Stripe is told: a refund answering an
// issuer's fraud report is fraudulent there, which Radar learns from.
func stripeRefundReason(p RefundPayload) string {
	switch p.RefundReason {
	case billing.RefundFraudWarning, billing.RefundFraudulent:
		return "fraudulent"
	case billing.RefundDuplicate:
		return "duplicate"
	case billing.RefundRequestedByCustomer:
		return "requested_by_customer"
	}
	return p.Reason
}
