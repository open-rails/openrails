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

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
)

// TypeTopupCharge is the prepaid auto-top-up card charge (#674): a durable
// write-ahead intent per top-up EPISODE. The episode identity derives from the
// persisted last_topup_at stamp (never wall clock), the provider order
// id/idempotency key derives from the intent id — a crash between charge and
// deposit re-resolves the SAME intent instead of re-charging.
const TypeTopupCharge = "topup_charge"

// TopupChargeIdempotencyKey addresses one top-up episode: the anchor is the
// account's last_topup_at (unix seconds, or "genesis"), which only advances
// when an episode finalizes or a decline stamps the cooldown.
func TopupChargeIdempotencyKey(customerID uuid.UUID, currency, episodeAnchor string) string {
	return fmt.Sprintf("%s:%s:%s:%s", TypeTopupCharge, customerID, strings.ToLower(strings.TrimSpace(currency)), episodeAnchor)
}

// TopupChargePayload is the stored payload for TypeTopupCharge.
type TopupChargePayload struct {
	CustomerID      uuid.UUID `json:"customer_id"`
	Currency        string    `json:"currency"`
	AmountNative    int64     `json:"amount_native"` // native micros; deposited verbatim
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
	EpisodeAnchor   string    `json:"episode_anchor"`
}

// topupWireRef is the provider-facing idempotency key / NMI order id — derived
// from the intent id (#674), stable across every execution attempt, ≤ 50 chars.
func topupWireRef(intentID uuid.UUID) string { return "topup:" + intentID.String() }

// TopupChargeHandler charges the saved payment method and deposits the
// purchased credits with money-mover semantics: the deposit's source_id IS the
// wire ref, so "did this episode land?" is answerable locally (deposit row)
// and at the provider (order-id sale search). Declines are terminal for the
// episode and stamp the cooldown; transport ambiguity parks for verification.
type TopupChargeHandler struct {
	DB      *db.DB
	Charger money.Charger
	// Resolver arms the intent merchant's NMI client from the armed rail
	// state for verification reads (#788).
	Resolver money.NMIClientResolver
	Clock    clockwork.Clock
	Policy   BackoffPolicy
}

func NewTopupChargeHandler(d *db.DB, charger money.Charger, resolver money.NMIClientResolver, clock clockwork.Clock) *TopupChargeHandler {
	return &TopupChargeHandler{DB: d, Charger: charger, Resolver: resolver, Clock: clock, Policy: DefaultBackoff}
}

func (h *TopupChargeHandler) Type() string                         { return TypeTopupCharge }
func (h *TopupChargeHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }

func (h *TopupChargeHandler) money() *money.MoneyService {
	return money.NewMoneyService(h.DB, h.Clock)
}

func decodeTopupChargePayload(intent gen.OpenrailsRailIntent) (TopupChargePayload, error) {
	var p TopupChargePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("topup charge intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode topup charge payload: %w", err)
	}
	if p.CustomerID == uuid.Nil || p.Currency == "" || p.AmountNative <= 0 || p.PaymentMethodID == uuid.Nil {
		return p, errors.New("topup charge payload is incomplete")
	}
	return p, nil
}

// Submitted episodes stay relevant even when settings or payment methods change:
// a known charge still has to be credited. Reservation checks all first sends.
func (h *TopupChargeHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	if _, err := decodeTopupChargePayload(intent); err != nil {
		return SupersededBy(err.Error()), nil
	}
	return StillRelevant(), nil
}

func topupEpisode(intent gen.OpenrailsRailIntent, p TopupChargePayload) money.AutoTopupEpisode {
	return money.AutoTopupEpisode{IntentID: intent.ID, CustomerID: p.CustomerID, PaymentMethodID: p.PaymentMethodID, Currency: p.Currency, Anchor: p.EpisodeAnchor, Amount: p.AmountNative}
}

func (h *TopupChargeHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	if h.Charger == nil {
		return Parked("off-session charger not configured")
	}
	p, err := decodeTopupChargePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	chargeMinor, err := moneyutil.NativeToRailMinor(p.Currency, p.AmountNative)
	if err != nil {
		return Terminal("top-up amount not representable in rail minor units: " + err.Error())
	}
	svc := h.money()
	existing, err := svc.ReserveAutoTopup(ctx, topupEpisode(intent, p))
	if errors.Is(err, money.ErrAutoTopupSafety) {
		return Parked(err.Error())
	}
	if err != nil {
		return Retryable("reserve top-up: " + err.Error())
	}
	if existing != nil {
		return h.Verify(ctx, intent)
	}
	res, err := h.Charger.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID: intent.MerchantID, Payer: identity.CustomerID(p.CustomerID), Invoker: p.CustomerID.String(), PaymentMethodID: p.PaymentMethodID,
		AmountCents: chargeMinor, Currency: p.Currency, IdempotencyKey: topupWireRef(intent.ID), Description: "auto top-up",
	})
	if err != nil {
		// Once submission starts, absence of a response is not proof of failure.
		// Neither retries nor expiry may create another charge for this episode.
		return Ambiguous("top-up submission requires exact provider verification: " + err.Error())
	}
	receipt := money.AutoTopupReceipt{TransactionID: res.TransactionID, Declined: res.Declined}
	if res.FailureMessage != nil {
		receipt.Reason = *res.FailureMessage
	}
	return h.recordAndFinalize(ctx, intent, p, receipt)
}

// Verify uses only exact local receipts or the provider's unique request ID.
// A missing provider search result does not prove a submitted payment failed.
func (h *TopupChargeHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeTopupChargePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	svc := h.money()
	ep, err := svc.GetAutoTopupEpisode(ctx, intent.ID)
	if err != nil {
		return Ambiguous("read top-up reservation: " + err.Error())
	}
	if ep == nil {
		return Ambiguous("top-up submission has no safety reservation; operator verification required")
	}
	if len(ep.Receipt) > 0 {
		return h.finalizeReceipt(ctx, intent, p)
	}
	if isNMIRail(intent.Rail) {
		transactionID, found, err := h.findNMISale(ctx, intent, intent.Rail, topupWireRef(intent.ID))
		if err != nil {
			return Ambiguous("provider verification failed: " + err.Error())
		}
		if found {
			return h.recordAndFinalize(ctx, intent, p, money.AutoTopupReceipt{TransactionID: transactionID})
		}
	}
	return Ambiguous("submitted top-up lacks an exact provider receipt; operator verification required, no automatic resend")
}

func (h *TopupChargeHandler) recordAndFinalize(ctx context.Context, intent gen.OpenrailsRailIntent, p TopupChargePayload, receipt money.AutoTopupReceipt) Outcome {
	if err := h.money().RecordAutoTopupReceipt(ctx, intent.ID, receipt); err != nil {
		out := Ambiguous("persist top-up provider receipt: " + err.Error())
		out.Evidence = map[string]any{"transaction_id": receipt.TransactionID, "declined": receipt.Declined, "reason": receipt.Reason}
		return out
	}
	return h.finalizeReceipt(ctx, intent, p)
}

func (h *TopupChargeHandler) finalizeReceipt(ctx context.Context, intent gen.OpenrailsRailIntent, p TopupChargePayload) Outcome {
	receipt, err := h.money().FinalizeAutoTopupReceipt(ctx, topupEpisode(intent, p))
	if err != nil {
		return Ambiguous("finalize durable top-up receipt: " + err.Error())
	}
	if receipt.Declined {
		return TerminalWithEvidence("top-up declined: "+receipt.Reason, map[string]any{"declined": true})
	}
	return Succeeded(map[string]any{"transaction_id": receipt.TransactionID, "deposited": true})
}

// findNMISale answers "did the charge land?" for an NMI-family rail via the
// Query API order-id search, using the intent merchant's armed NMI client.
func (h *TopupChargeHandler) findNMISale(ctx context.Context, intent gen.OpenrailsRailIntent, rail, wireRef string) (string, bool, error) {
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, intent)
	if err != nil {
		return "", false, err
	}
	if !ok || client == nil {
		return "", false, fmt.Errorf("nmi rail %q is not armed for this merchant", rail)
	}
	return client.FindSuccessfulSaleByOrderID(ctx, wireRef)
}

func isNMIRail(rail string) bool {
	return rails.IsNMI(models.Rail(rail))
}
