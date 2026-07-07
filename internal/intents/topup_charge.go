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
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
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

// CheckRelevance: a top-up whose account no longer has the auto-top-up
// configuration (settings removed, payment method gone) is superseded; the
// balance level is deliberately NOT re-checked — once the episode was cut, its
// charge either happened (must be deposited) or is cheap to complete.
func (h *TopupChargeHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := decodeTopupChargePayload(intent)
	if err != nil {
		return SupersededBy("unusable topup charge intent: " + err.Error()), nil
	}
	if _, err := h.DB.Gen(ctx).GetPaymentMethodByID(ctx, p.PaymentMethodID); err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("auto-topup payment method no longer exists"), nil
		}
		return Relevance{}, err
	}
	return StillRelevant(), nil
}

func (h *TopupChargeHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	if h.Charger == nil {
		return Parked("off-session charger not configured")
	}
	p, err := decodeTopupChargePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	wireRef := topupWireRef(intent.ID)
	svc := h.money()

	// Episode already deposited (crash after finalize, replayed executor)?
	if deposited, derr := svc.HasAutoTopupDeposit(ctx, p.CustomerID, p.Currency, wireRef); derr != nil {
		return Retryable("deposit lookup failed: " + derr.Error())
	} else if deposited {
		if serr := svc.StampAutoTopupAttempt(ctx, p.CustomerID, p.Currency); serr != nil {
			return Ambiguous("deposit exists but cooldown stamp failed: " + serr.Error())
		}
		return Succeeded(map[string]any{"deposited": true, "verified_existing": true})
	}

	rail, outcome := h.methodRail(ctx, p)
	if outcome != nil {
		return *outcome
	}

	// Money mover on a gateway without request idempotency (NMI): any
	// re-execution verifies by reading before sending another charge.
	if intent.Attempts > 1 && isNMIRail(rail) {
		txnID, found, verr := h.findNMISale(ctx, intent, rail, wireRef)
		if verr != nil {
			return Ambiguous("pre-send verification read failed: " + verr.Error())
		}
		if found {
			return h.finalize(ctx, svc, p, wireRef, txnID, true)
		}
	}

	chargeMinor, err := money.NativeToRailMinor(p.Currency, p.AmountNative)
	if err != nil {
		return Terminal("top-up amount not representable in rail minor units: " + err.Error())
	}
	res, err := h.Charger.ChargeSavedMethod(ctx, money.ChargeRequest{
		MerchantID:      intent.MerchantID,
		Payer:           identity.CustomerID(p.CustomerID),
		Invoker:         p.CustomerID.String(),
		PaymentMethodID: p.PaymentMethodID,
		AmountCents:     chargeMinor,
		Currency:        p.Currency,
		IdempotencyKey:  wireRef,
		Description:     "auto top-up",
	})
	if err != nil {
		if errors.Is(err, nmi.ErrProviderReadOnly) {
			return Parked("provider writes blocked (mode=readonly)")
		}
		if nmi.IsTransportAmbiguous(err) {
			return Ambiguous("top-up charge outcome unknown: " + err.Error())
		}
		// Clean transient failure. For Stripe the wire ref is the Stripe
		// Idempotency-Key, so a re-send replays rather than double-charges;
		// for NMI attempts>1 verifies by order id before re-sending.
		return Retryable("top-up charge failed: " + err.Error())
	}
	if res.Declined {
		// Terminal for the episode; stamp the cooldown so a failing card is
		// not hammered every tick (a later episode gets a fresh anchor).
		if serr := svc.StampAutoTopupAttempt(ctx, p.CustomerID, p.Currency); serr != nil {
			return Retryable("declined, but cooldown stamp failed: " + serr.Error())
		}
		reason := "top-up charge declined"
		if res.FailureMessage != nil && *res.FailureMessage != "" {
			reason = *res.FailureMessage
		}
		evidence := map[string]any{"declined": true}
		if res.FailureCode != nil {
			evidence["failure_code"] = *res.FailureCode
		}
		return TerminalWithEvidence(reason, evidence)
	}
	return h.finalize(ctx, svc, p, wireRef, res.TransactionID, false)
}

// Verify resolves an ambiguous top-up charge via reads: the local deposit row
// first, then the provider (NMI order-id sale search). Stripe needs no read —
// the wire ref is the Stripe Idempotency-Key, so re-execution replays safely.
func (h *TopupChargeHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeTopupChargePayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	wireRef := topupWireRef(intent.ID)
	svc := h.money()

	if deposited, derr := svc.HasAutoTopupDeposit(ctx, p.CustomerID, p.Currency, wireRef); derr != nil {
		return Ambiguous("deposit lookup failed: " + derr.Error())
	} else if deposited {
		if serr := svc.StampAutoTopupAttempt(ctx, p.CustomerID, p.Currency); serr != nil {
			return Ambiguous("deposit exists but cooldown stamp failed: " + serr.Error())
		}
		return Succeeded(map[string]any{"deposited": true, "verified_existing": true})
	}

	rail, outcome := h.methodRail(ctx, p)
	if outcome != nil {
		return *outcome
	}
	if isNMIRail(rail) {
		txnID, found, verr := h.findNMISale(ctx, intent, rail, wireRef)
		if verr != nil {
			return Ambiguous("provider read failed: " + verr.Error())
		}
		if !found {
			return Retryable("no successful sale found for wire ref; charge verified not executed")
		}
		return h.finalize(ctx, svc, p, wireRef, txnID, true)
	}
	// Stripe (and any idempotency-keyed rail): re-execution with the same key
	// replays the original charge — safe to hand back to the executor.
	return Retryable("rail replays by idempotency key; safe to re-execute")
}

// finalize deposits the confirmed charge (idempotent on source_id) and stamps
// the cooldown.
func (h *TopupChargeHandler) finalize(ctx context.Context, svc *money.MoneyService, p TopupChargePayload, wireRef, transactionID string, verified bool) Outcome {
	if err := svc.FinalizeAutoTopup(ctx, p.CustomerID, p.Currency, p.AmountNative, wireRef); err != nil {
		// The charge DID happen; the verifier re-runs finalize off the wire ref.
		return Ambiguous("top-up charged, but deposit/stamp failed: " + err.Error())
	}
	evidence := map[string]any{"transaction_id": transactionID, "deposited": true}
	if verified {
		evidence["verified_existing"] = true
	}
	return Succeeded(evidence)
}

// methodRail loads the payment method's rail; a missing method is superseded
// by relevance, so a load failure here is retryable.
func (h *TopupChargeHandler) methodRail(ctx context.Context, p TopupChargePayload) (string, *Outcome) {
	method, err := h.DB.Gen(ctx).GetPaymentMethodByID(ctx, p.PaymentMethodID)
	if err != nil {
		out := Retryable("load payment method: " + err.Error())
		return "", &out
	}
	return strings.ToLower(strings.TrimSpace(method.Rail)), nil
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
	return client.FindSuccessfulSaleByOrderID(wireRef)
}

func isNMIRail(rail string) bool {
	return rails.IsNMI(models.Rail(rail))
}
