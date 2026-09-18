package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/opsmetric"
)

// TypeManualRebill is the rail-side dunning charge (#358 phase C),
// folding the retired openrails.manual_rebill_attempts claim table into the
// ledger. One intent per (subscription, period end, attempt ordinal): the
// dunning worker enqueues + executes it synchronously and drives lifecycle
// (decline classification, FailMembership, the #359 retry schedule) off the
// returned status; declines are terminal PER ATTEMPT — the next scheduled
// retry is a new intent with the next ordinal.
//
// system-origin: dunning charges are proactive, so the gate parks them under
// limited/readonly and the relevance window (the #359 dunning window) expires
// them if the mode outlasts it.
const TypeManualRebill = "manual_rebill"

// ManualRebillPayload is the stored payload for TypeManualRebill.
type ManualRebillPayload struct {
	SubscriptionID uuid.UUID `json:"subscription_id"`
	PeriodEnd      time.Time `json:"period_end"`
	Rail           string    `json:"rail"`
	// OrderReference is stamped as the NMI order_id/ponumber: the correlation
	// handle the verifier queries by. Shared by every attempt for the period,
	// so verification answers "was this PERIOD charged?" — the invariant that
	// must never break.
	OrderReference string `json:"order_reference"`
	Attempt        int    `json:"attempt"`
	// The charge frozen before submission (#809 R4): the instrument and its
	// customer vault (the intent row names the provider account), and the
	// amount and currency the period is billed at. The verifier and operator
	// resolution accept only a provider sale matching every one of them, and
	// the renewal records exactly this amount.
	PaymentMethodID        uuid.UUID        `json:"payment_method_id"`
	Instrument             RebillInstrument `json:"instrument"`
	RailSubscriptionID     string           `json:"rail_subscription_id"`
	CredentialReference    string           `json:"credential_reference,omitempty"`
	CredentialAnchorSource string           `json:"credential_anchor_source"`
	Currency               string           `json:"currency"`
	Amount                 int64            `json:"amount"`
	AmountMinor            moneyutil.Cents  `json:"amount_minor"`
	// RequestKey binds a customer retry-now request (#809) to the operation
	// it started, and RequestPaymentMethodID is the method that request named
	// (nil = the subscription's current one): the same client key with a
	// different request is a conflict, never a replay. Empty on scheduled
	// attempts.
	RequestKey             string     `json:"request_key,omitempty"`
	RequestPaymentMethodID *uuid.UUID `json:"request_payment_method_id,omitempty"`
}

// Receipt is the exact provider sale this rebill must be confirmed by.
func (p ManualRebillPayload) Receipt() nmi.OrderSale {
	return nmi.OrderSale{OrderID: p.OrderReference, CustomerVaultID: p.Instrument.RailCustomerRef, Amount: p.AmountMinor, Currency: p.Currency}
}

// ManualRebillIdempotencyKey content-addresses one dunning charge attempt the
// same way the retired claim row was keyed (subscription + period end +
// rail + order reference), plus the attempt ordinal: a crash between
// charge and lifecycle update re-derives the SAME key (the failure count only
// moves when lifecycle moves) and gets the durable outcome back instead of
// double-charging, while each scheduled retry is a fresh intent.
func ManualRebillIdempotencyKey(subscriptionID uuid.UUID, periodEnd time.Time, rail, orderReference string, attempt int) string {
	return fmt.Sprintf("%s:%s:%d:%s:%s:attempt-%d",
		TypeManualRebill, subscriptionID, periodEnd.UTC().Unix(),
		strings.ToLower(strings.TrimSpace(rail)), strings.TrimSpace(orderReference), attempt)
}

// ManualRebillHandler implements the money-mover semantics for dunning
// charges: never blind-retry — a transport failure after the send parks as
// unknown_needs_verify and the verifier resolves it by querying NMI for the
// order reference; a clean gateway decline is terminal for the attempt with
// the response code preserved as evidence (the dunning worker classifies it
// hard/soft); confirmed success repairs the subscription lifecycle (renew +
// per-renewal credits) in finalize so the async drain and the late-confirming
// verifier need no waiting worker.
type ManualRebillHandler struct {
	DB     *db.DB
	Config *config.Config
	// Resolver arms the store-scoped NMI client per merchant AT CHARGE TIME
	// (#730/#788: the armed rail state is the ONLY credential plane;
	// declared-account-with-missing-secret fails closed; no caching).
	Resolver NMIClientResolver
	Clock    clockwork.Clock
	Policy   BackoffPolicy
}

func NewManualRebillHandler(d *db.DB, cfg *config.Config, resolver NMIClientResolver, clock clockwork.Clock) *ManualRebillHandler {
	return &ManualRebillHandler{DB: d, Config: cfg, Resolver: resolver, Clock: clock, Policy: DefaultBackoff}
}

// railClient arms the NMI client for one charge from the armed rail state
// (scope = the intent's stamped provenance account — dunning stamps the
// subscription's account, archived stays chargeable for existing
// obligations). An account that cannot arm errors (fail closed).
func (h *ManualRebillHandler) railClient(ctx context.Context, intent gen.OpenrailsRailIntent) (*nmi.NMIClient, error) {
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil {
		return nil, err
	}
	if !ok || client == nil {
		return nil, fmt.Errorf("nmi rail is not armed for provider %q", intent.Rail)
	}
	return client, nil
}

func (h *ManualRebillHandler) Type() string                         { return TypeManualRebill }
func (h *ManualRebillHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

// PrunePolicy keeps the frozen payload: a customer retry-now key replays
// against the operation's request_key after success (#809).
func (h *ManualRebillHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, true }

func decodeManualRebillPayload(intent gen.OpenrailsRailIntent) (ManualRebillPayload, error) {
	var p ManualRebillPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("manual rebill intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode manual rebill payload: %w", err)
	}
	if p.SubscriptionID == uuid.Nil || p.PeriodEnd.IsZero() || strings.TrimSpace(p.OrderReference) == "" ||
		p.PaymentMethodID == uuid.Nil || p.Instrument.RailMethodRef == "" || p.RailSubscriptionID == "" || strings.TrimSpace(p.Instrument.RailCustomerRef) == "" ||
		strings.TrimSpace(p.Currency) == "" || p.Amount <= 0 || p.AmountMinor <= 0 {
		return p, errors.New("manual rebill payload is incomplete")
	}
	if intent.PspID != nil && p.Instrument.PSPID != *intent.PspID {
		return p, errors.New("frozen rebill instrument belongs to another provider account")
	}
	if err := p.Instrument.Validate(); err != nil {
		return p, fmt.Errorf("manual rebill payload: %w", err)
	}
	return p, nil
}

// CheckRelevance: a dunning charge applies while the subscription is still
// past_due ON THE SAME period. Recovery (webhook rebill, user fix), terminal
// cancellation or a period advance all supersede; the dunning window itself
// is the intent's expires_at, enforced by the executor's expiry sweep.
func (h *ManualRebillHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	if intent.Attempts > 1 || intent.Status == StatusUnknownNeedsVerify {
		return StillRelevant(), nil
	}
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return SupersededBy("unusable manual rebill intent: " + err.Error()), nil
	}
	sub, err := subscriptions.NewSubscriptionRepo(h.DB).GetByID(ctx, p.SubscriptionID)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("subscription row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if sub.Status != models.StatusPastDue {
		return SupersededBy(fmt.Sprintf("subscription no longer past_due (status=%s)", sub.Status)), nil
	}
	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.UTC().Unix() != p.PeriodEnd.UTC().Unix() {
		return SupersededBy("billing period advanced past the dunned period"), nil
	}
	paid, err := h.DB.Gen(ctx).HasCompletedPaymentAtOrAfterPeriodEnd(ctx, gen.HasCompletedPaymentAtOrAfterPeriodEndParams{
		MerchantID:     intent.MerchantID,
		SubscriptionID: p.SubscriptionID,
		PeriodEnd:      *sub.CurrentPeriodEndsAt,
	})
	if err != nil {
		return Relevance{}, fmt.Errorf("check dunned period payment: %w", err)
	}
	if paid {
		return SupersededBy("billing period already has a completed payment"), nil
	}
	if reason, err := h.pinInstrument(ctx, intent, p); err != nil || reason != "" {
		if err != nil {
			return Relevance{}, err
		}
		return SupersededBy(reason), nil
	}
	return StillRelevant(), nil
}

// pinInstrument re-establishes, under the frozen instrument's shared row lock
// (it conflicts with the #297 custody remap's FOR UPDATE, which also refuses
// while this intent is in flight), that the charge can go out exactly as it
// was frozen: the subscription still bills this instrument, the instrument
// still names the frozen vault, and the subscription, the intent and the
// instrument are one provider account (#657 same-PSP invariant). A non-empty
// reason supersedes the intent; re-enqueueing the period's attempt revives it
// with a fresh freeze once the state is repaired. Nothing here reaches the
// provider.
func (h *ManualRebillHandler) pinInstrument(ctx context.Context, intent gen.OpenrailsRailIntent, p ManualRebillPayload) (reason string, err error) {
	if intent.PspID == nil {
		return "rebill is not addressed to a provider account", nil
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		method, lerr := gen.New(tx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: intent.MerchantID, ID: p.PaymentMethodID})
		if errors.Is(lerr, pgx.ErrNoRows) {
			reason = "the frozen payment method no longer exists"
			return nil
		}
		if lerr != nil {
			return fmt.Errorf("lock frozen payment method: %w", lerr)
		}
		sub, serr := subscriptions.NewSubscriptionRepo(h.DB.NewWithPgxTx(tx)).GetByID(ctx, p.SubscriptionID)
		if serr != nil {
			return fmt.Errorf("load subscription: %w", serr)
		}
		switch {
		case sub.PaymentMethodID == nil || *sub.PaymentMethodID != p.PaymentMethodID:
			reason = "the subscription's payment method changed since the rebill was frozen"
		case sub.RailSubscriptionID != p.RailSubscriptionID:
			reason = "the subscription's provider reference changed since the rebill was frozen"
		case method.RailMethodRef != p.Instrument.RailMethodRef:
			reason = "the payment method's billing reference changed since the rebill was frozen"
		case sub.PspID != *intent.PspID || method.PspID != *intent.PspID:
			reason = fmt.Sprintf("%s: subscription PSP %s, rebill PSP %s, payment method PSP %s; a cross-PSP rebill is never sent (#657)", "payment_method_psp_mismatch", sub.PspID, *intent.PspID, method.PspID)
		default:
			if err := p.Instrument.Matches(method); err != nil {
				reason = "instrument_changed: " + err.Error()
			}
		}
		return nil
	})
	return reason, err
}

func (h *ManualRebillHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	if intent.Attempts > 1 {
		return h.Verify(ctx, intent)
	}
	client, err := h.railClient(ctx, intent)
	if err != nil {
		// Unarmable (unconfigured, or declared-but-secretless — fail closed):
		// park, never charge; the executor drains it once the operator repairs.
		return Parked(err.Error())
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}

	// The frozen instrument, never the subscription's current link:
	// CheckRelevance already superseded a drifted one under its row lock.
	pm, err := paymentmethods.NewPaymentMethodRepo(h.DB).GetByID(ctx, p.PaymentMethodID)
	if err != nil && !errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return Parked("load frozen payment method before submission: " + err.Error())
	}
	if pm != nil && (intent.PspID == nil || p.Instrument.PSPID != *intent.PspID || p.Instrument.Matches(gen.OpenrailsPaymentMethod{ID: pm.ID, PspID: pm.PspID, Custodian: pm.Custodian, CustodianID: pm.CustodianID, RailCustomerRef: pm.RailCustomerRef, RailMethodRef: pm.RailMethodRef}) != nil) {
		return Parked("frozen payment method no longer matches the rebill; the relevance re-check supersedes it")
	}
	if pm == nil || pm.RailCustomerRef == "" || pm.RailMethodRef == "" {
		// Terminal for the attempt: nothing was sent. The worker (or the next
		// dunning pass, off this evidence) applies the failure policy.
		// NMI needs both the customer vault (rail_customer_ref) and the billing
		// record (rail_method_ref) to rebill.
		return TerminalWithEvidence("payment method unavailable for rebill", map[string]any{"declined": false})
	}

	// #297: a dunning retry is a merchant-initiated RECURRING charge — carry
	// the credential-on-file indicators plus the approved recurring sequence
	// anchor whenever it was captured. Historical rows without an anchor still
	// get a best-effort merchant+used MIT so a missing migration artifact does
	// not strand the subscription; the exception is explicit in logs/evidence.
	priorRef := p.CredentialReference
	anchorSource := p.CredentialAnchorSource
	if priorRef == "" {
		anchorSource = "unavailable"
		log.WithContext(ctx).WithFields(log.Fields{
			"intent_id":         intent.ID,
			"subscription_id":   p.SubscriptionID,
			"payment_method_id": pm.ID,
			"order_reference":   p.OrderReference,
		}).Warn("manual rebill: sending best-effort MIT without the stored-credential anchor")
		opsmetric.Emit(ctx, opsmetric.MetricNMIUnanchoredMIT, log.Fields{
			"transport":         "direct_subscription",
			"payment_method_id": pm.ID,
			"agreement":         charge.AgreementRecurring,
		})
	} else if anchorSource == "legacy_initial_transaction_id" {
		log.WithContext(ctx).WithFields(log.Fields{
			"intent_id":         intent.ID,
			"subscription_id":   p.SubscriptionID,
			"payment_method_id": pm.ID,
		}).Warn("manual rebill: using legacy unscoped initial transaction ID as the stored-credential anchor")
	}
	anchorMissing := priorRef == ""
	credentialContext := charge.RecurringMIT(priorRef)
	if anchorMissing {
		credentialContext = charge.LegacyUnanchoredRecurringMIT()
	}

	rebillResp, err := client.AttemptManualRebill(ctx, nmi.ManualRebillParams{
		VaultID:          p.Instrument.RailCustomerRef,
		BillingID:        p.Instrument.RailMethodRef,
		SubscriptionID:   p.RailSubscriptionID,
		OrderID:          p.OrderReference,
		PONumber:         p.OrderReference,
		StoredCredential: nmidirect.StoredCredentialFor(credentialContext),
	})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return Parked("nmi provider writes blocked (mode=readonly)")
		}
		// The charge may or may not have reached the gateway; exactly the old
		// markManualRebillUnknown posture, now resolved by the verifier.
		return Ambiguous("manual rebill request failed: " + err.Error())
	}
	if rebillResp == nil || !rebillResp.Success {
		reason := "rebill declined"
		responseCode := 0
		if rebillResp != nil {
			if rebillResp.ErrorMessage != "" {
				reason = rebillResp.ErrorMessage
			}
			responseCode = rebillResp.ResponseCode
		}
		if nmi.UncertainResponseCode(responseCode) {
			return Ambiguous("rebill response requires verification: " + reason)
		}
		return TerminalWithEvidence(reason, map[string]any{
			"declined":                         true,
			"response_code":                    responseCode,
			"stored_credential_anchor_missing": anchorMissing,
			"stored_credential_anchor_source":  anchorSource,
		})
	}

	// Classic approval supplies a candidate ID only: recurring rebills do not
	// send amount/currency and NMI's current plan may differ from the freeze.
	if err := h.saveRebillCandidate(ctx, intent, rebillResp.TransactionID); err != nil {
		return AmbiguousWithEvidence("rebill approved, but candidate custody failed: "+err.Error(), map[string]any{"transaction_id": rebillResp.TransactionID})
	}
	out := h.confirmAndFinalizeRebill(ctx, intent, p, client, rebillResp.TransactionID)
	if out.Evidence == nil {
		out.Evidence = map[string]any{}
	}
	out.Evidence["stored_credential_anchor_missing"] = anchorMissing
	out.Evidence["stored_credential_anchor_source"] = anchorSource
	return out
}

// Verify accepts only a persisted bound receipt or exact provider readback.
// A saved transaction ID is a candidate; it never authorizes renewal itself.
func (h *ManualRebillHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return Ambiguous(err.Error())
	}
	if receipt, found, err := loadRebillReceipt(intent, p); err != nil {
		return rebillContradicted(err)
	} else if found {
		return h.completeRebill(ctx, intent, p, receipt)
	}
	client, err := h.railClient(ctx, intent)
	if err != nil {
		return Ambiguous("nmi client unavailable, cannot verify: " + err.Error())
	}
	candidate := EvidenceString(intent, rebillCandidateKey)
	if candidate == "" {
		candidate = EvidenceString(intent, "transaction_id")
	}
	return h.confirmAndFinalizeRebill(ctx, intent, p, client, candidate)
}

// rebillEvidenceContradiction retains a provider sale found under the
// operation's order reference that is NOT the frozen charge.
const rebillEvidenceContradiction = "provider_contradiction"

func rebillContradicted(err error) Outcome {
	return AmbiguousWithEvidence("provider receipt contradicts the frozen rebill: "+err.Error(), map[string]any{rebillEvidenceContradiction: err.Error()})
}

// finalizeSuccess records a confirmed rebill charge exactly once, whatever
// dunning did to the subscription meanwhile. The renewal (payment row, period
// advance, access window) is deduped on the transaction id, so a retried
// finalize is a no-op:
//
//	past_due / unknown (dunning parked it) / active -> RenewMembership
//	terminally cancelled (user, merchant, chargeback) -> payment row only;
//	                                    never reactivated, flagged for refund review
func (h *ManualRebillHandler) finalizeSuccess(ctx context.Context, intent gen.OpenrailsRailIntent, p ManualRebillPayload, receipt manualRebillReceipt) error {
	if err := receipt.matches(intent, p); err != nil {
		return err
	}
	return h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		database := h.DB.NewWithPgxTx(tx)
		return h.finalizeRebillInTx(ctx, database, intent, p, receipt)
	})
}

func (h *ManualRebillHandler) finalizeRebillInTx(ctx context.Context, database *db.DB, intent gen.OpenrailsRailIntent, p ManualRebillPayload, receipt manualRebillReceipt) error {
	transactionID := receipt.transactionID
	subRepo := subscriptions.NewSubscriptionRepo(database)
	sub, err := subRepo.GetByIDForUpdate(ctx, p.SubscriptionID)
	if err != nil {
		return fmt.Errorf("load subscription: %w", err)
	}

	if intent.PspID == nil || sub.PspID != *intent.PspID || sub.RailSubscriptionID != p.RailSubscriptionID {
		return errors.New("subscription no longer matches the qualified receipt's provider identity")
	}

	priceSvc := catalog.NewPriceService(database)
	productSvc := catalog.NewProductService(database)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(
		database, productSvc, priceSvc,
		entitlements.NewEntitlementService(database, h.Clock),
		subscriptions.NewNotificationService(database, nil),
		payments.NewPaymentService(database, h.Clock),
		h.Clock,
	)
	lifecycle.SetConfig(h.Config)

	// The confirmed charge is the frozen one: its amount, never a price read
	// at confirmation time.
	params := &subscriptions.RenewMembershipParams{
		Rail:               models.Rail(strings.ToLower(intentRail(p, sub))),
		RailSubscriptionID: p.RailSubscriptionID,
		TransactionID:      transactionID,
		Amount:             p.Amount,
		Currency:           p.Currency,
	}
	if _, terminal := subscriptions.TerminalCancelReason(sub); terminal {
		return lifecycle.RecordConfirmedChargeWithoutRenewal(ctx, params)
	}
	if sub.Status != models.StatusPastDue {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": sub.ID, "status": sub.Status, "transaction_id": transactionID, "dunned_period_end": p.PeriodEnd,
		}).Warn("manual rebill confirmed after the subscription left past_due; renewing from the confirmed charge")
	}
	if err := lifecycle.RenewMembership(ctx, params); err != nil {
		return fmt.Errorf("renew membership: %w", err)
	}
	return nil
}

// intentRail prefers the payload's rail (what the producer charged
// under) and falls back to the subscription row.
func intentRail(p ManualRebillPayload, sub *models.Subscription) string {
	if proc := strings.TrimSpace(p.Rail); proc != "" {
		return proc
	}
	return string(sub.Rail)
}
