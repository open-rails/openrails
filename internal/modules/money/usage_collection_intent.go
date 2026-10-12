package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// TypeOrderCollection is one off-session charge of a saved payment method
// for one usage order. The intent id is the provider identity (NMI order id,
// Stripe idempotency-key root), so every execution of one operation reuses
// one provider reference. After a possible submission the operation stays
// unknown until an exact receipt (NMI order search, Stripe idempotent replay,
// or operator resolution) or provider-confirmed non-execution; nothing here
// ever resends under a new identity.
const TypeOrderCollection = intents.TypeOrderCollection

// Evidence keys retained on the operation.
const (
	collectionEvidenceTransactionID = "transaction_id"
	collectionEvidenceExternalID    = "external_invoice_id"
	collectionEvidenceRail          = "rail"
	collectionEvidenceDeclined      = "declined"
	collectionEvidenceFailureCode   = "failure_code"
	collectionEvidenceFailureMsg    = "failure_message"
	collectionEvidenceNotExecuted   = "not_executed"
	// collectionEvidenceFailureCodeNotExecuted says why a never-charged
	// operation ended: not_executed (provider-confirmed) or
	// instrument_changed (refused before submission).
	collectionEvidenceFailureCodeNotExecuted = "not_executed_code"
	collectionEvidenceSubmittedAt            = "submitted_at"
	// collectionEvidenceContradiction retains a provider object found under
	// this operation's identity that is NOT the frozen charge. Nothing settles
	// from it and non-execution can no longer be attested: an operator must
	// repair from the provider record.
	collectionEvidenceContradiction = "provider_contradiction"
)

// stripeReplayWindow bounds idempotent replay of a Stripe collection to well
// inside Stripe's 24h idempotency-key retention: a replay after the key
// expired would create a second Stripe invoice.
const stripeReplayWindow = 23 * time.Hour

// OrderCollectionHandler executes and reconciles order_collection intents.
type OrderCollectionHandler struct {
	DB       *db.DB
	Charger  Charger
	Verifier CollectionVerifier
	Config   intents.ModeView
	Clock    clockwork.Clock
	Policy   intents.BackoffPolicy
}

func NewOrderCollectionHandler(database *db.DB, charger Charger, verifier CollectionVerifier, cfg intents.ModeView, clock clockwork.Clock) *OrderCollectionHandler {
	return &OrderCollectionHandler{DB: database, Charger: charger, Verifier: verifier, Config: cfg, Clock: timeutil.FirstClock(clock), Policy: intents.DefaultBackoff}
}

func (h *OrderCollectionHandler) Type() string { return TypeOrderCollection }
func (h *OrderCollectionHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}
func (h *OrderCollectionHandler) now() time.Time { return h.Clock.Now().UTC() }

// PrunePolicy keeps the frozen payload: a client retry key replays against
// the operation's identity (order, customer, payment method) after success.
func (h *OrderCollectionHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, true }

// Terminal state is committed with the local order/ledger/outbox effects.
func (h *OrderCollectionHandler) CommitsTerminalOutcome() bool { return true }

// CheckRelevance: a collection operation is never superseded. The order's
// live attempt blocks every competing mutation while the operation lives, and a
// possibly submitted charge must reconcile, so only a terminal outcome (or an
// operator release of a never-submitted operation) ends it.
func (h *OrderCollectionHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}

func (h *OrderCollectionHandler) Execute(ctx context.Context, intent gen.BillingProviderIntent) intents.Outcome {
	current, err := intents.NewStore(h.DB).Get(ctx, intent.ID)
	if err != nil {
		return intents.Ambiguous("load canonical collection progress: " + err.Error())
	}
	intent = current
	p, err := intents.DecodeOrderCollectionPayload(intent)
	if err != nil {
		return intents.Parked(err.Error())
	}
	if outcome, done := h.finalizeFromEvidence(ctx, intent, p); done {
		return outcome
	}
	// Admission is not authority to send forever: recheck the frozen
	// collection owner under the order lock before dispatch or replay.
	var owned bool
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := h.DB.NewWithPgxTx(tx).Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: intent.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		var err error
		owned, err = collectionOwned(ctx, q, intent, p)
		return err
	})
	if err != nil {
		return intents.Parked("check order collection authority: " + err.Error())
	}
	if !owned {
		return intents.Parked("order is not owned by this engine collection")
	}
	if intents.EvidenceString(intent, collectionEvidenceSubmittedAt) != "" {
		// Resumed after a possible submission. Stripe replays its idempotent
		// sequence; NMI-family rails may only read.
		if p.Rail == string(models.RailStripe) {
			return h.replayStripe(ctx, intent, p)
		}
		return h.Verify(ctx, intent)
	}
	if outcome, done := h.recoverBeforeDispatch(ctx, intent, p); done {
		return outcome
	}
	if h.Charger == nil {
		return intents.Parked("order collection charger not wired")
	}
	prepared, err := h.Charger.Prepare(ctx, h.chargeRequest(intent, p))
	if errors.Is(err, charge.ErrInstrumentChanged) {
		// Nothing was sent and the frozen instrument is gone: release the
		// order so the next attempt freezes the method as it now is.
		return h.finalizeNotExecuted(ctx, intent, p, "instrument_changed", err.Error())
	}
	if err != nil {
		// Nothing reached the provider: park without consuming the attempt.
		return intents.Parked("collection not armed: " + err.Error())
	}
	// Write-ahead fence: from here on the charge may exist at the provider.
	store := intents.NewStore(h.DB)
	proof, first, err := store.BeginCollectedPayment(ctx, intent, h.now())
	if err != nil {
		return intents.Parked("record submission fence: " + err.Error())
	}
	if !first {
		return h.Verify(ctx, intent)
	}

	res, err := prepared.Submit(ctx)
	if errors.Is(err, charge.ErrInstrumentChanged) {
		// Only this executor owns the freshly inserted fence, and the scoped
		// charger raises this refusal before entering its provider callback.
		// An existing marker never reaches this path or authorizes nonexecution.
		return h.finalizeNotExecuted(ctx, intent, p, "instrument_changed", err.Error(), proof)
	}
	if errors.Is(err, charge.ErrNotDispatched) {
		// This fresh owner alone knows its call never entered a provider POST.
		// Resumed operations branch to Verify before reaching Submit above.
		return h.finalizeNotExecuted(ctx, intent, p, "not_dispatched", charge.ErrNotDispatched.Error(), proof)
	}
	return h.classify(ctx, intent, p, res, err)
}

func (h *OrderCollectionHandler) chargeRequest(intent gen.BillingProviderIntent, p intents.OrderCollectionPayload) ChargeRequest {
	orderID := p.OrderID
	return ChargeRequest{
		MerchantID:          intent.MerchantID,
		Payer:               identity.CustomerID(p.CustomerID),
		Invoker:             p.CustomerID.String(),
		OrderID:             &orderID,
		PaymentMethodID:     p.PaymentMethodID,
		AmountCents:         p.AmountMinor,
		Currency:            p.Currency,
		IdempotencyKey:      intent.ID.String(),
		Description:         p.Description,
		Instrument:          p.Instrument,
		HyperSwitch:         p.HyperSwitch,
		Initiator:           p.Initiator,
		ProviderCustomerRef: p.ProviderCustomerRef,
	}
}

// classify turns one submission answer into the operation outcome: a parsed
// refusal is terminal, a receipt settles, every error is a possible
// submission.
func (h *OrderCollectionHandler) classify(ctx context.Context, intent gen.BillingProviderIntent, p intents.OrderCollectionPayload, res ChargeResult, err error) intents.Outcome {
	if errors.Is(err, subscriptions.ErrStripeReceiptMismatch) {
		return contradicted(err)
	}
	if err != nil {
		return intents.Ambiguous("collection outcome unknown: " + err.Error())
	}
	rail := normalizeRail(res.Rail)
	if rail == "" {
		rail = p.Rail
	}
	if res.Declined {
		return h.finalizeRefusal(ctx, intent, p, rail, derefStr(res.FailureCode), derefStr(res.FailureMessage), res.TransactionID)
	}
	if strings.TrimSpace(res.TransactionID) == "" {
		return intents.Ambiguous("successful collection response has no transaction receipt")
	}
	candidate := intents.CollectionCandidate{TransactionID: res.TransactionID, ExternalInvoiceID: res.ExternalInvoiceID}
	if err := intents.NewStore(h.DB).RetainCollectionCandidate(ctx, intent, candidate); err != nil {
		return intents.Ambiguous("retain provider candidate: " + err.Error())
	}
	reference := candidate.TransactionID
	if p.Rail == "stripe" {
		reference = candidate.ExternalInvoiceID
	}
	return h.qualifyAndSettle(ctx, intent, reference)
}

// replayStripe re-sends the same idempotent sequence while Stripe still holds
// the key; beyond that window only an exact receipt resolves the operation.
func (h *OrderCollectionHandler) replayStripe(ctx context.Context, intent gen.BillingProviderIntent, p intents.OrderCollectionPayload) intents.Outcome {
	if !h.stripeReplayable(intent) {
		return intents.Ambiguous("stripe idempotency window elapsed; resolve with the exact stripe invoice or provider-confirmed non-execution")
	}
	if h.Charger == nil {
		return intents.Ambiguous("order collection charger not wired; cannot replay")
	}
	prepared, err := h.Charger.Prepare(ctx, h.chargeRequest(intent, p))
	if err != nil {
		return intents.Ambiguous("stripe replay not armed: " + err.Error())
	}
	res, err := prepared.Submit(ctx)
	return h.classify(ctx, intent, p, res, err)
}

func (h *OrderCollectionHandler) stripeReplayable(intent gen.BillingProviderIntent) bool {
	submitted, err := time.Parse(time.RFC3339Nano, intents.EvidenceString(intent, collectionEvidenceSubmittedAt))
	if err != nil {
		submitted = intent.CreatedAt
	}
	return h.now().Before(submitted.Add(stripeReplayWindow))
}

// Verify reconciles a submitted operation from retained evidence or a
// positive provider read. An empty search never authorizes another send.
func (h *OrderCollectionHandler) Verify(ctx context.Context, intent gen.BillingProviderIntent) intents.Outcome {
	current, err := intents.NewStore(h.DB).Get(ctx, intent.ID)
	if err != nil {
		return intents.Ambiguous("load canonical collection progress: " + err.Error())
	}
	intent = current
	p, err := intents.DecodeOrderCollectionPayload(intent)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if outcome, done := h.finalizeFromEvidence(ctx, intent, p); done {
		return outcome
	}
	if intents.EvidenceString(intent, collectionEvidenceSubmittedAt) == "" {
		// An older book can retain the accepted operation but predate its
		// submission marker. Positive same-order evidence still belongs to the
		// canonical writer; an empty read grants no nonexecution authority.
		if p.Rail == string(models.RailNMI) && h.Verifier != nil {
			receipt, found, err := h.Verifier.ReadCollectionReceipt(ctx, intent, "")
			if err != nil {
				return intents.Ambiguous("read accepted collection receipt: " + err.Error())
			}
			if found {
				return h.finalizeSettle(ctx, intent, receipt)
			}
		}
		return intents.Retryable("collection has no submission fence; resume execution")
	}
	if p.Rail == string(models.RailStripe) {
		if !h.stripeReplayable(intent) {
			return intents.Ambiguous("stripe idempotency window elapsed; resolve with the exact stripe invoice or provider-confirmed non-execution")
		}
		if blocked, reason := intents.GateExecution(ctx, h.Config, intent.MerchantID, intents.Origin(intent.Origin)); blocked {
			return intents.Ambiguous("stripe replay deferred: " + reason)
		}
		// The executor replays through the provider-enforced idempotency key.
		return intents.Retryable("stripe collection replay through provider idempotency key")
	}
	return h.qualifyAndSettle(ctx, intent, "")
}

// Resolve applies operator evidence: an exact provider object confirmed as
// this operation's settled charge, or provider-confirmed non-execution. It
// never re-sends.
func (h *OrderCollectionHandler) Resolve(ctx context.Context, intent gen.BillingProviderIntent, resolution intents.Resolution) (intents.Outcome, error) {
	p, err := intents.DecodeOrderCollectionPayload(intent)
	if err != nil {
		return intents.Outcome{}, err
	}
	if _, found, err := intents.LoadCollectedReceipt(intent); err != nil {
		return intents.Outcome{}, intents.RejectResolution("stored receipt is invalid; operation requires repair: %v", err)
	} else if found {
		return intents.Outcome{}, intents.RejectResolution("operation holds a qualified receipt; verifier must complete settlement")
	}
	if resolution.Step != "" {
		return intents.Outcome{}, fmt.Errorf("%w: an order collection has no steps", intents.ErrResolutionInvalid)
	}
	if receipt := intents.EvidenceString(intent, collectionEvidenceTransactionID); receipt != "" {
		return intents.Outcome{}, intents.RejectResolution("operation already holds receipt %s; its verifier completes settlement", receipt)
	}
	if _, found, err := intents.LoadCollectionNonexecution(intent); err != nil || found {
		return intents.Outcome{}, intents.RejectResolution("operation holds nonexecution custody; its verifier must complete it")
	}
	if intents.EvidenceString(intent, collectionEvidenceFailureCode) != "" {
		return intents.Outcome{}, intents.RejectResolution("operation already holds a definitive provider answer; its verifier completes it")
	}
	if h.Verifier == nil {
		return intents.Outcome{}, errors.New("no collection verifier wired")
	}
	if resolution.NotExecuted {
		if contradiction := intents.EvidenceString(intent, collectionEvidenceContradiction); contradiction != "" {
			return intents.Outcome{}, intents.RejectResolution("provider evidence contradicts this operation (%s); non-execution cannot be attested, repair from the provider record", contradiction)
		}
		proof, err := intents.NewStore(h.DB).ConfirmCollectedPaymentNotExecuted(ctx, intent, h.Verifier)
		if err != nil {
			return intents.Outcome{}, intents.RejectResolution("%v", err)
		}
		return h.finalizeNotExecuted(ctx, intent, p, "", "", proof), nil
	}
	receipt, found, err := h.Verifier.ReadCollectionReceipt(ctx, intent, resolution.ProviderReference)
	if err != nil {
		return intents.Outcome{}, intents.RejectResolution("%v", err)
	}
	if !found {
		return intents.Outcome{}, intents.RejectResolution("provider object is not this operation's settled charge")
	}
	return h.finalizeSettle(ctx, intent, receipt), nil
}

// contradicted keeps an operation unknown when the provider holds an object
// under its identity that is not the frozen charge.
func contradicted(err error) intents.Outcome {
	return intents.AmbiguousWithEvidence("provider receipt contradicts the frozen operation: "+err.Error(), map[string]any{collectionEvidenceContradiction: err.Error()})
}

// ResolveUnsent releases a pending operation that never reached the provider:
// the write-ahead fence is absent, so nothing can have been charged. The
// attempt fails without a decline and the order becomes due again. An
// operation carrying the fence belongs to its verifier.
func (h *OrderCollectionHandler) ResolveUnsent(ctx context.Context, intent gen.BillingProviderIntent, resolution intents.Resolution) (intents.Outcome, error) {
	current, err := intents.NewStore(h.DB).Get(ctx, intent.ID)
	if err != nil {
		return intents.Outcome{}, err
	}
	intent = current
	p, err := intents.DecodeOrderCollectionPayload(intent)
	if err != nil {
		return intents.Outcome{}, err
	}
	if _, found, err := intents.LoadCollectedReceipt(intent); err != nil || found {
		return intents.Outcome{}, intents.RejectResolution("operation holds receipt custody; it cannot be declared unsubmitted")
	}
	if !resolution.NotExecuted || resolution.Step != "" {
		return intents.Outcome{}, fmt.Errorf("%w: a never-submitted collection accepts only --not-executed", intents.ErrResolutionInvalid)
	}
	if intents.EvidenceString(intent, collectionEvidenceSubmittedAt) != "" {
		return intents.Outcome{}, intents.RejectResolution("operation %s crossed its submission fence; only its verifier or an unknown-state resolution can close it", intent.ID)
	}
	return h.finalizeNotExecuted(ctx, intent, p, "not_executed", "collection was not submitted"), nil
}

// finalizeFromEvidence retries local effects for an answer the operation
// already holds (a receipt, a parsed refusal, confirmed non-execution).
func (h *OrderCollectionHandler) finalizeFromEvidence(ctx context.Context, intent gen.BillingProviderIntent, p intents.OrderCollectionPayload) (intents.Outcome, bool) {
	rail := intents.EvidenceString(intent, collectionEvidenceRail)
	if rail == "" {
		rail = p.Rail
	}
	nonexecution, hasNonexecution, err := intents.LoadCollectionNonexecution(intent)
	if err != nil {
		return intents.Ambiguous("stored nonexecution rejected: " + err.Error()), true
	}
	receipt, hasReceipt, err := intents.LoadCollectedReceipt(intent)
	if err != nil {
		return intents.Ambiguous("stored receipt rejected: " + err.Error()), true
	}
	if hasReceipt && hasNonexecution {
		return intents.Ambiguous("qualified payment contradicts qualified nonexecution"), true
	}
	if hasReceipt {
		return h.finalizeSettle(ctx, intent, receipt), true
	}
	if hasNonexecution {
		code, reason := nonexecution.Refusal()
		return h.finalizeNotExecuted(ctx, intent, p, code, reason), true
	}
	if candidate, found, err := intents.LoadCollectionCandidate(intent); err != nil {
		return intents.Ambiguous(err.Error()), true
	} else if found {
		reference := candidate.TransactionID
		if p.Rail == "stripe" {
			reference = candidate.ExternalInvoiceID
		}
		return h.qualifyAndSettle(ctx, intent, reference), true
	}

	if intent.Status == intents.StatusFailedTerminal && intents.EvidenceString(intent, collectionEvidenceNotExecuted) != "" {
		code := intents.EvidenceString(intent, collectionEvidenceFailureCodeNotExecuted)
		return h.finalizeNotExecuted(ctx, intent, p, code, intents.EvidenceString(intent, collectionEvidenceFailureMsg)), true
	}
	if code := intents.EvidenceString(intent, collectionEvidenceFailureCode); code != "" {
		return h.finalizeRefusal(ctx, intent, p, rail, code, intents.EvidenceString(intent, collectionEvidenceFailureMsg), ""), true
	}
	return intents.Outcome{}, false
}

// finalizeSettle records one confirmed charge exactly once: the arrears
// liability settled on the ledger (deduped on the operation key), the
// payment, the usage order paid and its attempt ended, all or nothing. A
// local failure retains the receipt on the operation for the verifier and
// keeps the order's collection live.
func (h *OrderCollectionHandler) finalizeSettle(ctx context.Context, intent gen.BillingProviderIntent, receipt intents.CollectedReceipt) intents.Outcome {
	p, err := intents.DecodeOrderCollectionPayload(intent)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	retained, err := intents.NewStore(h.DB).RetainCollectedReceipt(ctx, intent, receipt)
	if err != nil {
		return intents.Ambiguous("retain qualified receipt before local settlement: " + err.Error())
	}
	transactionID, externalInvoiceID, rail := retained.TransactionID(), retained.ExternalInvoiceID(), p.Rail
	evidence := map[string]any{collectionEvidenceTransactionID: transactionID, collectionEvidenceRail: rail, "verified_existing": true}
	if ext := strings.TrimSpace(externalInvoiceID); ext != "" {
		evidence[collectionEvidenceExternalID] = ext
	}
	now := h.now()
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		complete := func() error {
			return intents.NewStore(h.DB.NewWithPgxTx(tx)).CompleteOrderCollection(ctx, intent, intents.Succeeded(evidence), now)
		}
		chargedAmount, err := moneyutil.RailMinorToNative(p.Currency, p.AmountMinor)
		if err != nil || chargedAmount < p.Amount {
			return fmt.Errorf("collection charge does not cover the frozen order amount")
		}
		// Take the payer's money lock before order/ledger writes. Rounding
		// credits use the same spendable-lot path and lock order as deposits.
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: intent.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		settled, err := orderPaymentRecorded(ctx, q, intent, p)
		if err != nil {
			return err
		}
		if settled {
			return complete()
		}
		if owned, err := collectionOwned(ctx, q, intent, p); err != nil {
			return err
		} else if !owned {
			return fmt.Errorf("order %s collection already ended without a payment; confirmed charge %s needs repair", p.OrderID, transactionID)
		}
		if strings.TrimSpace(transactionID) == "" {
			return errors.New("collection receipt names no provider transaction")
		}
		// An approved storing charge establishes the card's agreements only from
		// retained provider custody and in the settlement transaction.
		storing := p.Initiator == charge.InitiatorCustomer && p.Instrument.Mandate == nil
		if storing {
			method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: intent.MerchantID, ID: p.PaymentMethodID})
			if err != nil {
				return err
			}
			if err := p.Instrument.Matches(method); err != nil {
				return err
			}
		}
		currency := normalizeCurrency(p.Currency)
		ml := ledger.New(q, intent.MerchantID)
		coord := ledger.Coord{Operation: ledger.OpOrderPayment, Source: "invoice_charge", SourceID: intent.IdempotencyKey}
		transfer, err := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: intent.MerchantID, CustomerID: p.CustomerID, Currency: currency,
			TransferType: "owed_payment", Operation: string(coord.Operation), Source: coord.Source, SourceID: coord.SourceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			orderID := p.OrderID
			transfer, err = ml.PayOwed(ctx, p.CustomerID, currency, p.Amount, coord, &orderID)
		}
		if err != nil {
			return err
		}
		if excess := chargedAmount - p.Amount; excess > 0 {
			payer := identity.CustomerIDFromString(p.CustomerID.String())
			key := "invoice-rounding:" + intent.ID.String()
			reason := "Rounding credit from order " + billing.OrderID(p.OrderID).String()
			_, err = NewMoneyService(h.DB, h.Clock).depositTx(ctx, q, DepositParams{
				CustomerID: &payer, Currency: currency, Amount: excess,
				Source: "invoice_rounding_purchase", SourceID: &key, Description: &reason,
			})
			if err != nil {
				return fmt.Errorf("credit collection rounding surplus: %w", err)
			}
		}
		if err := q.InsertOrderPayment(ctx, gen.InsertOrderPaymentParams{
			ID: p.PaymentID, MerchantID: intent.MerchantID, CustomerID: p.CustomerID, OrderID: p.OrderID, LedgerTransferID: transfer.ID,
			Channel: string(models.ChannelRail), Rail: &rail, PspID: &p.Instrument.PSPID, TransactionID: transactionID,
			Amount: chargedAmount, Currency: currency, PaidAt: now, Now: now, TokenType: optionalString(payments.DefaultTokenType(rail, p.Instrument.Custodian)),
		}); err != nil {
			return fmt.Errorf("record order payment: %w", err)
		}
		if err := recordCollectionAttempt(ctx, q, intent, p, rail, attempts.Attempt{Approved: true, TransactionID: transactionID}, now); err != nil {
			return err
		}
		if storing {
			// Another storing charge on this card may have settled first; its
			// lineage stands and this payment still counts.
			if err := mandates.RecordStored(ctx, q, mandates.Stored{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, PaymentMethodID: p.PaymentMethodID, PSPID: p.Instrument.PSPID,
				Rail: rail, Currency: currency, Lineage: charge.Mandate{InitialTransactionID: transactionID}, AcceptedAt: now}); err != nil {
				return err
			}
		}
		attempt := intent.ID
		paid, err := usage.Pay(ctx, q, intent.MerchantID, p.OrderID, p.Amount, &p.PaymentID, &attempt, now)
		if err != nil {
			// The order changed under its own live collection (only raw
			// surgery can do that). Fail closed: the receipt stays on the
			// operation for an operator to repair.
			return fmt.Errorf("order %s no longer accepts the frozen amount %d; confirmed charge %s needs repair: %w", p.OrderID, p.Amount, transactionID, err)
		}
		if p.Initiator == charge.InitiatorMerchant {
			if err := queueUsageReceipt(ctx, q, intent.MerchantID, paid, p.PaymentID, chargedAmount, currency, now); err != nil {
				return err
			}
		}
		if err := usage.EndCollection(ctx, q, intent.MerchantID, p.OrderID, intent.ID, true, nil, now); err != nil {
			return err
		}
		return complete()
	})
	if err != nil {
		return intents.AmbiguousWithEvidence("collection charged, but local settlement failed: "+err.Error(), evidence)
	}
	return intents.Succeeded(evidence)
}

// finalizeRefusal records a definitive provider refusal: attempt failed, the
// usage order dunned on its own billing cycle and open again (or
// uncollectible), with why. A local failure retains the refusal for the
// verifier.
func (h *OrderCollectionHandler) finalizeRefusal(ctx context.Context, intent gen.BillingProviderIntent, p intents.OrderCollectionPayload, rail, failureCode, failureMessage, transactionID string) intents.Outcome {
	if strings.TrimSpace(failureCode) == "" {
		failureCode = "declined"
	}
	evidence := map[string]any{collectionEvidenceDeclined: true, collectionEvidenceFailureCode: failureCode, collectionEvidenceRail: rail}
	if failureMessage != "" {
		evidence[collectionEvidenceFailureMsg] = failureMessage
	}
	now := h.now()
	var action collection.Action
	var order *gen.BillingOrder
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: intent.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		complete := func() error {
			return intents.NewStore(h.DB.NewWithPgxTx(tx)).CompleteOrderCollection(ctx, intent, intents.TerminalWithEvidence("collection refused: "+failureCode, evidence), now)
		}
		if settled, err := orderPaymentRecorded(ctx, q, intent, p); err != nil {
			return err
		} else if settled {
			return fmt.Errorf("payment %s already settled; refusal contradicts a recorded charge", p.PaymentID)
		}
		row, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: intent.MerchantID, ID: p.OrderID})
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		if !collectionLive(row, intent.ID) {
			return complete()
		}
		order = &row
		code := failureCode
		action, err = collection.FailureAction(usageCycleHours(row), rail, &code, int(derefInt32(row.CollectionFailureCount)), row.CollectionFailedAt, now)
		if err != nil {
			return err
		}
		if err := recordCollectionAttempt(ctx, q, intent, p, rail, attempts.Attempt{Answer: decline.Evidence{Code: failureCode, Text: failureMessage}, TransactionID: transactionID}, now); err != nil {
			return err
		}
		failure := billing.DeclineReason(decline.ReasonFor(rail, failureCode)).Failure()
		if err := usage.DeclineCollection(ctx, q, intent.MerchantID, p.OrderID, intent.ID, failure, action.Terminal, action.NextAttemptAt, now); err != nil {
			return err
		}
		if err := queueCollectionOutcome(ctx, h.DB.NewWithPgxTx(tx), row, action, failureCode, now); err != nil {
			return err
		}
		return complete()
	})
	if errors.Is(err, collection.ErrUnknownCycle) {
		if ferr := collection.RecordUnknownCycle(ctx, h.DB.Gen(ctx), intent.MerchantID, p.OrderID.String(), "order", err); ferr != nil {
			log.WithContext(ctx).WithError(ferr).WithField("order_id", p.OrderID).Error("order collection: could not record the unknown-cycle finding")
		}
	}
	if err != nil {
		return intents.AmbiguousWithEvidence("collection refused, but local record failed: "+err.Error(), evidence)
	}
	if order != nil {
		logCollectionDecline(ctx, p.OrderID, rail, &failureCode, action)
	}
	return intents.TerminalWithEvidence("collection refused: "+failureCode, evidence)
}

// finalizeNotExecuted records a collection that provably never charged: the
// attempt fails without a decline and the order is due again immediately.
// code/reason default to provider-confirmed non-execution.
func (h *OrderCollectionHandler) finalizeNotExecuted(ctx context.Context, intent gen.BillingProviderIntent, p intents.OrderCollectionPayload, code, reason string, proof ...intents.CollectionNonexecutionProof) intents.Outcome {
	if code == "" {
		code, reason = "not_executed", "provider confirmed the collection was not executed"
	}
	if len(proof) != 0 {
		if len(proof) != 1 {
			return intents.Ambiguous("invalid nonexecution capability")
		}
		if err := intents.NewStore(h.DB).RetainCollectionNonexecution(ctx, intent, proof[0], code, reason); err != nil {
			return intents.Ambiguous("retain nonexecution proof: " + err.Error())
		}
	}
	evidence := map[string]any{collectionEvidenceNotExecuted: true, collectionEvidenceDeclined: false,
		collectionEvidenceFailureCodeNotExecuted: code, collectionEvidenceFailureMsg: reason}
	now := h.now()
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: intent.MerchantID, ID: p.CustomerID}); err != nil {
			return err
		}
		complete := func() error {
			return intents.NewStore(h.DB.NewWithPgxTx(tx)).CompleteOrderCollection(ctx, intent, intents.TerminalWithEvidence(reason, evidence), now)
		}
		if settled, err := orderPaymentRecorded(ctx, q, intent, p); err != nil {
			return err
		} else if settled {
			return fmt.Errorf("payment %s already settled; non-execution contradicts a recorded charge", p.PaymentID)
		}
		if owned, err := collectionOwned(ctx, q, intent, p); err != nil {
			return err
		} else if !owned {
			return complete()
		}
		if err := usage.EndCollection(ctx, q, intent.MerchantID, p.OrderID, intent.ID, false, &now, now); err != nil {
			return err
		}
		return complete()
	})
	if err != nil {
		// Only sealed custody can survive a failed local transaction. Never
		// promote an arbitrary or stale outcome map to replayable proof.
		return intents.Ambiguous("collection nonexecution could not commit: " + err.Error())
	}
	return intents.TerminalWithEvidence(reason, evidence)
}

// usageCycleHours is the order's own billing cycle for the dunning schedule:
// its period, floored at the shortest retriable cycle (a threshold order can
// cover an hour and must not be written off on its first decline). 0 =
// unknown, which the schedule refuses (ErrUnknownCycle).
func usageCycleHours(order gen.BillingOrder) int {
	if order.PeriodStartsAt == nil || order.PeriodEndsAt == nil {
		return 0
	}
	cycleHours := collection.CycleHoursBetween(*order.PeriodStartsAt, *order.PeriodEndsAt)
	if cycleHours > 0 && cycleHours < collection.MinRetryCycleHours {
		return collection.MinRetryCycleHours
	}
	return cycleHours
}

// logCollectionDecline names the bucket and its consequence on every
// refusal. Nothing here cancels anything.
func logCollectionDecline(ctx context.Context, orderID uuid.UUID, rail string, failureCode *string, action collection.Action) {
	decline.AlertUnmapped(ctx, action.Decline)
	entry := log.WithContext(ctx).WithFields(log.Fields{
		"order_id": orderID, "rail": rail, "failure_code": derefStr(failureCode),
		"decline_outcome": action.Decline.Action.String(), "decline_coverage": string(action.Decline.Coverage),
	})
	switch {
	case action.AwaitingPaymentMethod():
		entry.Warn("order collection: the customer's card needs fixing; charging stops, the order stays open")
	case action.Terminal && !action.ScheduleExhausted():
		entry.Error("order collection: non-recoverable decline; the order is uncollectible")
	case action.Terminal:
		entry.Error("order collection: the retry schedule is exhausted; the order is uncollectible")
	default:
		entry.WithField("next_attempt_at", action.NextAttemptAt).Warn("order collection: retryable decline; it retries on the order's own billing cycle")
	}
}

// queueCollectionOutcome records the customer's notice in the same domain
// transaction; no external message is sent here.
func queueCollectionOutcome(ctx context.Context, database *db.DB, order gen.BillingOrder, action collection.Action, failureCode string, now time.Time) error {
	amountDue := usage.Due(order)
	data := billing.NotificationData{
		OrderID: billing.OrderID(order.ID), Currency: order.Currency, AmountDue: &amountDue,
		FailureCode: failureCode, DeclineOutcome: action.Decline.Action.String(),
	}
	eventType := models.NotificationPaymentMethodFailed
	switch {
	case action.AwaitingPaymentMethod():
		eventType = models.NotificationPaymentMethodUpdateRequired
	case action.Terminal:
		eventType = models.NotificationOrderCollectionStopped
		reason := "non_recoverable"
		if action.ScheduleExhausted() {
			reason = "schedule_exhausted"
		}
		data.Reason = reason
	case action.NextAttemptAt != nil:
		next := action.NextAttemptAt.UTC()
		data.NextAttemptAt = &next
	}
	notification := &models.NotificationEvent{ID: uuidutil.NewV7(), CustomerID: order.CustomerID, EventType: eventType, Data: data, CreatedAt: now}
	return subscriptions.NewNotificationService(database).Create(ctx, notification)
}

func (h *OrderCollectionHandler) qualifyAndSettle(ctx context.Context, in gen.BillingProviderIntent, reference string) intents.Outcome {
	if h.Verifier == nil {
		return intents.Ambiguous("no collection receipt reader wired")
	}
	receipt, found, err := h.Verifier.ReadCollectionReceipt(ctx, in, reference)
	if errors.Is(err, nmi.ErrReceiptMismatch) || errors.Is(err, subscriptions.ErrStripeReceiptMismatch) {
		return contradicted(err)
	}
	if err != nil {
		return intents.Ambiguous("qualify provider receipt: " + err.Error())
	}
	if !found {
		return intents.Ambiguous("no exact provider receipt; no automatic resend")
	}
	return h.finalizeSettle(ctx, in, receipt)
}

// recordCollectionAttempt records the collection charge's answer as a
// payment attempt on the order, in the transaction that settles or fails it.
func recordCollectionAttempt(ctx context.Context, q *gen.Queries, in gen.BillingProviderIntent, p intents.OrderCollectionPayload, rail string, a attempts.Attempt, at time.Time) error {
	if rail == "" {
		rail = in.Rail
	}
	a.MerchantID, a.CustomerID, a.PSPID, a.Rail, a.Kind, a.At, a.Step = in.MerchantID, p.CustomerID, p.Instrument.PSPID, rail, attempts.Collection, at, "charge"
	a.Amount, a.Currency, a.PaymentMethodID, a.ProviderIntentID, a.OrderID = p.Amount, p.Currency, &p.PaymentMethodID, &in.ID, &p.OrderID
	a.TokenType = payments.DefaultTokenType(rail, p.Instrument.Custodian)
	a.Sent = p.Instrument.Mandate
	return attempts.Record(ctx, q, a)
}

// orderPaymentRecorded reports whether the collection's payment is written:
// the charge settled the order.
func orderPaymentRecorded(ctx context.Context, q *gen.Queries, in gen.BillingProviderIntent, p intents.OrderCollectionPayload) (bool, error) {
	_, err := q.GetOrderPayment(ctx, gen.GetOrderPaymentParams{MerchantID: in.MerchantID, CustomerID: p.CustomerID, OrderID: p.OrderID, ID: p.PaymentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// collectionOwned locks the order and reports whether this collection is
// still its live attempt; one that ended released it.
func collectionOwned(ctx context.Context, q *gen.Queries, in gen.BillingProviderIntent, p intents.OrderCollectionPayload) (bool, error) {
	row, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: in.MerchantID, ID: p.OrderID})
	if err != nil {
		return false, fmt.Errorf("lock order: %w", err)
	}
	return collectionLive(row, in.ID), nil
}

// collectionLive reports whether operation is the order's live collection.
func collectionLive(order gen.BillingOrder, operation uuid.UUID) bool {
	return order.CollectionOwner == string(billing.OrderCollectionEngine) && order.Origin == string(billing.OrderOriginUsage) &&
		order.Status == string(billing.OrderProcessing) && order.AttemptID != nil && *order.AttemptID == operation
}

func derefInt32(v *int32) int32 {
	if v == nil {
		return 0
	}
	return *v
}

// queueUsageReceipt queues the receipt of a usage order charged under the
// customer's collection agreement, once per payment.
func queueUsageReceipt(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, order gen.BillingOrder, payment uuid.UUID, amount int64, currency string, now time.Time) error {
	data := billing.NotificationData{OrderID: billing.OrderID(order.ID), PaymentID: billing.PaymentID(payment), Amount: &amount, Currency: currency}
	if order.Number != nil {
		data.OrderNumber = *order.Number
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{
		ID:         uuidutil.DeterministicID(uuidutil.DeterministicNamespace, string(models.NotificationUsageCharged), merchantID.String(), payment.String()),
		MerchantID: merchantID, CustomerID: order.CustomerID, EventType: string(models.NotificationUsageCharged), Data: raw, CreatedAt: now,
	})
}
