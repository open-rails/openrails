package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// TypeManualRebill is the processor-side dunning charge (#358 phase C),
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
	Processor      string    `json:"processor"`
	// OrderReference is stamped as the NMI order_id/ponumber: the correlation
	// handle the verifier queries by. Shared by every attempt for the period,
	// so verification answers "was this PERIOD charged?" — the invariant that
	// must never break.
	OrderReference string `json:"order_reference"`
	Attempt        int    `json:"attempt"`
}

// ManualRebillIdempotencyKey content-addresses one dunning charge attempt the
// same way the retired claim row was keyed (subscription + period end +
// processor + order reference), plus the attempt ordinal: a crash between
// charge and lifecycle update re-derives the SAME key (the failure count only
// moves when lifecycle moves) and gets the durable outcome back instead of
// double-charging, while each scheduled retry is a fresh intent.
func ManualRebillIdempotencyKey(subscriptionID uuid.UUID, periodEnd time.Time, processor, orderReference string, attempt int) string {
	return fmt.Sprintf("%s:%s:%d:%s:%s:attempt-%d",
		TypeManualRebill, subscriptionID, periodEnd.UTC().Unix(),
		strings.ToLower(strings.TrimSpace(processor)), strings.TrimSpace(orderReference), attempt)
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
	DB       *db.DB
	Config   *config.Config
	Clients  map[string]*nmi.NMIClient
	Clock    clockwork.Clock
	EventLog *analytics.EventLogService
	Policy   BackoffPolicy
}

func NewManualRebillHandler(d *db.DB, cfg *config.Config, clients map[string]*nmi.NMIClient, clock clockwork.Clock, eventLog *analytics.EventLogService) *ManualRebillHandler {
	return &ManualRebillHandler{DB: d, Config: cfg, Clients: clients, Clock: clock, EventLog: eventLog, Policy: DefaultBackoff}
}

func (h *ManualRebillHandler) Type() string                         { return TypeManualRebill }
func (h *ManualRebillHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

func decodeManualRebillPayload(intent gen.BillingProviderIntent) (ManualRebillPayload, error) {
	var p ManualRebillPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("manual rebill intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode manual rebill payload: %w", err)
	}
	if p.SubscriptionID == uuid.Nil || p.PeriodEnd.IsZero() || strings.TrimSpace(p.OrderReference) == "" {
		return p, errors.New("manual rebill payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: a dunning charge applies while the subscription is still
// past_due ON THE SAME period. Recovery (webhook rebill, user fix), terminal
// cancellation or a period advance all supersede; the dunning window itself
// is the intent's expires_at, enforced by the executor's expiry sweep.
func (h *ManualRebillHandler) CheckRelevance(ctx context.Context, intent gen.BillingProviderIntent) (Relevance, error) {
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return SupersededBy("unusable manual rebill intent: " + err.Error()), nil
	}
	sub, err := repo.NewSubscriptionRepo(h.DB).GetByID(ctx, p.SubscriptionID)
	if err != nil {
		if repo.IsNotFound(err) {
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
	return StillRelevant(), nil
}

func (h *ManualRebillHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	client, ok := h.Clients[strings.ToLower(intent.Provider)]
	if !ok || client == nil {
		return Parked(fmt.Sprintf("nmi client not configured for provider %q", intent.Provider))
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only (mode=readonly)")
	}
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}

	// Money mover on a gateway without request idempotency: any re-execution
	// (lease reclaim, retry after a clean failure) verifies by reading before
	// sending another charge.
	if intent.Attempts > 1 {
		txnID, found, verr := h.findSuccessfulSale(client, p)
		if verr != nil {
			return Ambiguous("pre-send verification read failed: " + verr.Error())
		}
		if found {
			if err := h.finalizeSuccess(ctx, p, txnID); err != nil {
				return Ambiguous("charge verified at provider, but local lifecycle repair failed: " + err.Error())
			}
			return Succeeded(map[string]any{"transaction_id": txnID, "verified_existing": true})
		}
	}

	sub, err := repo.NewSubscriptionRepo(h.DB).GetByID(ctx, p.SubscriptionID)
	if err != nil {
		return Retryable("load subscription: " + err.Error())
	}
	pm := sub.PaymentMethod
	if pm == nil || pm.VaultID == "" || pm.BillingID == nil || *pm.BillingID == "" {
		// Terminal for the attempt: nothing was sent. The worker (or the next
		// dunning pass, off this evidence) applies the failure policy.
		return TerminalWithEvidence("payment method unavailable for rebill", map[string]any{"declined": false})
	}

	rebillResp, err := client.AttemptManualRebill(nmi.ManualRebillParams{
		VaultID:        pm.VaultID,
		BillingID:      *pm.BillingID,
		SubscriptionID: sub.ProcessorSubscriptionID,
		OrderID:        p.OrderReference,
		PONumber:       p.OrderReference,
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
		return TerminalWithEvidence(reason, map[string]any{
			"declined":      true,
			"response_code": responseCode,
		})
	}

	if err := h.finalizeSuccess(ctx, p, rebillResp.TransactionID); err != nil {
		// The charge DID happen; route through the verifier so the lifecycle
		// repair is retried (its read re-finds the sale by order reference).
		return Ambiguous("rebill charged, but local lifecycle update failed: " + err.Error())
	}
	return Succeeded(map[string]any{"transaction_id": rebillResp.TransactionID})
}

// Verify resolves an ambiguous charge via the Query API: a successful sale
// for the order reference means the period was charged (whenever that
// happened) — finalize repairs the lifecycle and the intent succeeds; a clean
// read with no successful sale means no money moved and the executor may
// retry this attempt.
func (h *ManualRebillHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) Outcome {
	client, ok := h.Clients[strings.ToLower(intent.Provider)]
	if !ok || client == nil {
		return Ambiguous(fmt.Sprintf("nmi client not configured for provider %q; cannot verify", intent.Provider))
	}
	p, err := decodeManualRebillPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	txnID, found, err := h.findSuccessfulSale(client, p)
	if err != nil {
		return Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return Retryable("no successful sale found for order reference; charge verified not executed")
	}
	if err := h.finalizeSuccess(ctx, p, txnID); err != nil {
		return Ambiguous("charge verified at provider, but local lifecycle repair failed: " + err.Error())
	}
	return Succeeded(map[string]any{"transaction_id": txnID, "verified_existing": true})
}

// findSuccessfulSale queries NMI for transactions carrying the period's order
// reference and reports the first successful sale. Every attempt for the
// period shares the order reference, so a hit from ANY attempt counts — that
// is the no-double-charge invariant. The probe itself lives on the NMI client
// (nmi.FindSuccessfulSaleByOrderID) so the #367 liveness sync shares it.
func (h *ManualRebillHandler) findSuccessfulSale(client *nmi.NMIClient, p ManualRebillPayload) (transactionID string, found bool, err error) {
	return client.FindSuccessfulSaleByOrderID(p.OrderReference)
}

// finalizeSuccess repairs the local lifecycle from a confirmed successful
// rebill: renew the membership window (payment row, period advance,
// notifications) and grant per-renewal credits. Idempotent — a subscription
// that already left past_due (the renewal applied, or a racing worker
// repaired it) is left alone.
func (h *ManualRebillHandler) finalizeSuccess(ctx context.Context, p ManualRebillPayload, transactionID string) error {
	subRepo := repo.NewSubscriptionRepo(h.DB)
	sub, err := subRepo.GetByID(ctx, p.SubscriptionID)
	if err != nil {
		return fmt.Errorf("load subscription: %w", err)
	}
	if sub.Status != models.StatusPastDue ||
		sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.UTC().Unix() != p.PeriodEnd.UTC().Unix() {
		return nil
	}

	priceSvc := catalog.NewPriceService(h.DB)
	productSvc := catalog.NewProductService(h.DB)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(
		h.DB, productSvc, priceSvc,
		entitlements.NewEntitlementService(h.DB, h.Clock),
		subscriptions.NewNotificationService(h.DB, nil),
		payments.NewPaymentService(h.DB, h.Clock),
		h.EventLog, h.Clock,
	)
	lifecycle.SetConfig(h.Config)

	amount := int64(0)
	currency := subscriptions.CurrencyUSD
	if sub.Price != nil {
		amount = sub.Price.Amount
		currency = sub.Price.Currency
	} else if price, perr := priceSvc.GetByID(ctx, sub.PriceID); perr == nil {
		amount = price.Amount
		currency = price.Currency
	}

	if err := lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Processor:               models.Processor(strings.ToLower(intentProcessor(p, sub))),
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		TransactionID:           transactionID,
		Amount:                  amount,
		Currency:                currency,
	}); err != nil {
		return fmt.Errorf("renew membership: %w", err)
	}

	// Credits mirror the dunning worker's post-renew grant: failures are
	// logged, never failed — the renewal itself is the money-critical part.
	if updated, err := h.DB.Gen(ctx).GetSubscriptionByID(ctx, p.SubscriptionID); err != nil {
		log.WithContext(ctx).WithError(err).Warn("manual rebill finalize: load subscription after renew for credit grants")
	} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
		if err := money.NewMoneyService(h.DB, h.Clock).GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
			SubscriptionID: p.SubscriptionID,
			PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
			Cadence:        models.CreditGrantCadencePerRenewal,
			Source:         "subscription_renewal",
		}); err != nil {
			log.WithContext(ctx).WithError(err).Warn("manual rebill finalize: grant subscription credits after successful rebill")
		}
	}
	return nil
}

// intentProcessor prefers the payload's processor (what the producer charged
// under) and falls back to the subscription row.
func intentProcessor(p ManualRebillPayload, sub *models.Subscription) string {
	if proc := strings.TrimSpace(p.Processor); proc != "" {
		return proc
	}
	return string(sub.Processor)
}
