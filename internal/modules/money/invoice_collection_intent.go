package money

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

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// TypeInvoiceCollection is one off-session charge of a saved payment method
// for one invoice. The intent id is the provider identity (NMI order id,
// Stripe idempotency-key root), so every execution of one operation reuses
// one provider reference. After a possible submission the operation stays
// unknown until an exact receipt (NMI order search, Stripe idempotent replay,
// or operator resolution) or provider-confirmed non-execution; nothing here
// ever resends under a new identity.
const TypeInvoiceCollection = "invoice_collection"

// InvoiceCollectionPayload freezes the charge before submission. The amount
// is the invoice's amount_due at enqueue, in native precision and rail minor
// units; replays never recalculate it.
type InvoiceCollectionPayload struct {
	InvoiceID       uuid.UUID       `json:"invoice_id"`
	CustomerID      uuid.UUID       `json:"customer_id"`
	AttemptID       uuid.UUID       `json:"attempt_id"`
	PaymentMethodID uuid.UUID       `json:"payment_method_id"`
	Rail            string          `json:"rail"`
	Currency        string          `json:"currency"`
	Amount          int64           `json:"amount"`
	AmountMinor     moneyutil.Cents `json:"amount_minor"`
	Description     string          `json:"description"`
}

// Evidence keys retained on the operation.
const (
	collectionEvidenceTransactionID = "transaction_id"
	collectionEvidenceExternalID    = "external_invoice_id"
	collectionEvidenceRail          = "rail"
	collectionEvidenceDeclined      = "declined"
	collectionEvidenceFailureCode   = "failure_code"
	collectionEvidenceFailureMsg    = "failure_message"
	collectionEvidenceNotExecuted   = "not_executed"
	collectionEvidenceSubmittedAt   = "submitted_at"
)

// stripeReplayWindow bounds idempotent replay of a Stripe collection to well
// inside Stripe's 24h idempotency-key retention: a replay after the key
// expired would create a second invoice.
const stripeReplayWindow = 23 * time.Hour

// InvoiceCollectionHandler executes and reconciles invoice_collection intents.
type InvoiceCollectionHandler struct {
	DB       *db.DB
	Charger  Charger
	Verifier CollectionVerifier
	Config   intents.ModeView
	Clock    clockwork.Clock
	Policy   intents.BackoffPolicy
}

func NewInvoiceCollectionHandler(database *db.DB, charger Charger, verifier CollectionVerifier, cfg intents.ModeView, clock clockwork.Clock) *InvoiceCollectionHandler {
	return &InvoiceCollectionHandler{DB: database, Charger: charger, Verifier: verifier, Config: cfg, Clock: timeutil.FirstClock(clock), Policy: intents.DefaultBackoff}
}

func (h *InvoiceCollectionHandler) Type() string { return TypeInvoiceCollection }
func (h *InvoiceCollectionHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}
func (h *InvoiceCollectionHandler) now() time.Time { return h.Clock.Now().UTC() }

// PrunePolicy keeps the frozen payload: a client retry key replays against
// the operation's identity (invoice, customer, payment method) after success.
func (h *InvoiceCollectionHandler) PrunePolicy() (keepPayload, keepEvidence bool) { return true, false }

func decodeInvoiceCollectionPayload(intent gen.OpenrailsRailIntent) (InvoiceCollectionPayload, error) {
	var p InvoiceCollectionPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("invoice collection intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode invoice collection payload: %w", err)
	}
	if p.InvoiceID == uuid.Nil || p.CustomerID == uuid.Nil || p.AttemptID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.Amount <= 0 || p.AmountMinor <= 0 || p.Currency == "" {
		return p, errors.New("invoice collection payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: a collection operation is never superseded. The invoice's
// pointer blocks every competing mutation while the operation lives, and a
// possibly submitted charge must reconcile, so only a terminal outcome (or an
// operator release of a never-submitted operation) ends it.
func (h *InvoiceCollectionHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}

func (h *InvoiceCollectionHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeInvoiceCollectionPayload(intent)
	if err != nil {
		return intents.Parked(err.Error())
	}
	if outcome, done := h.finalizeFromEvidence(ctx, intent, p); done {
		return outcome
	}
	if intent.Attempts > 1 || intents.EvidenceString(intent, collectionEvidenceSubmittedAt) != "" {
		// Resumed after a possible submission. Stripe replays its idempotent
		// sequence; NMI-family rails may only read.
		if p.Rail == string(models.RailStripe) {
			return h.replayStripe(ctx, intent, p)
		}
		return h.Verify(ctx, intent)
	}
	if h.Charger == nil {
		return intents.Parked("invoice collection charger not wired")
	}
	prepared, err := h.Charger.Prepare(ctx, h.chargeRequest(intent, p))
	if err != nil {
		// Nothing reached the provider: park without consuming the attempt.
		return intents.Parked("collection not armed: " + err.Error())
	}
	// Write-ahead fence: from here on the charge may exist at the provider.
	store := intents.NewStore(h.DB)
	first, err := store.RecordProgressIfAbsent(ctx, intent.ID, collectionEvidenceSubmittedAt, h.now().Format(time.RFC3339Nano))
	if err != nil {
		return intents.Parked("record submission fence: " + err.Error())
	}
	if !first {
		return h.Verify(ctx, intent)
	}
	res, err := prepared.Submit(ctx)
	return h.classify(ctx, intent, p, res, err)
}

func (h *InvoiceCollectionHandler) chargeRequest(intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload) ChargeRequest {
	invoiceID := p.InvoiceID
	return ChargeRequest{
		MerchantID:      intent.MerchantID,
		Payer:           identity.CustomerID(p.CustomerID),
		Invoker:         p.CustomerID.String(),
		InvoiceID:       &invoiceID,
		PaymentMethodID: p.PaymentMethodID,
		AmountCents:     p.AmountMinor,
		Currency:        p.Currency,
		IdempotencyKey:  intent.ID.String(),
		Description:     p.Description,
	}
}

// classify turns one submission answer into the operation outcome: a parsed
// refusal is terminal, a receipt settles, every error is a possible
// submission.
func (h *InvoiceCollectionHandler) classify(ctx context.Context, intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload, res ChargeResult, err error) intents.Outcome {
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
	return h.finalizeSettle(ctx, intent, p, rail, res.TransactionID, res.ExternalInvoiceID, false)
}

// replayStripe re-sends the same idempotent sequence while Stripe still holds
// the key; beyond that window only an exact receipt resolves the operation.
func (h *InvoiceCollectionHandler) replayStripe(ctx context.Context, intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload) intents.Outcome {
	if !h.stripeReplayable(intent) {
		return intents.Ambiguous("stripe idempotency window elapsed; resolve with the exact stripe invoice or provider-confirmed non-execution")
	}
	if h.Charger == nil {
		return intents.Ambiguous("invoice collection charger not wired; cannot replay")
	}
	prepared, err := h.Charger.Prepare(ctx, h.chargeRequest(intent, p))
	if err != nil {
		return intents.Ambiguous("stripe replay not armed: " + err.Error())
	}
	res, err := prepared.Submit(ctx)
	return h.classify(ctx, intent, p, res, err)
}

func (h *InvoiceCollectionHandler) stripeReplayable(intent gen.OpenrailsRailIntent) bool {
	submitted, err := time.Parse(time.RFC3339Nano, intents.EvidenceString(intent, collectionEvidenceSubmittedAt))
	if err != nil {
		submitted = intent.CreatedAt
	}
	return h.now().Before(submitted.Add(stripeReplayWindow))
}

// Verify reconciles a submitted operation from retained evidence or a
// positive provider read. An empty search never authorizes another send.
func (h *InvoiceCollectionHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeInvoiceCollectionPayload(intent)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if outcome, done := h.finalizeFromEvidence(ctx, intent, p); done {
		return outcome
	}
	if p.Rail == string(models.RailStripe) {
		if !h.stripeReplayable(intent) {
			return intents.Ambiguous("stripe idempotency window elapsed; resolve with the exact stripe invoice or provider-confirmed non-execution")
		}
		if blocked, reason := intents.GateExecution(h.Config, intents.Origin(intent.Origin)); blocked {
			return intents.Ambiguous("stripe replay deferred: " + reason)
		}
		// The executor replays through the provider-enforced idempotency key.
		return intents.Retryable("stripe collection replay through provider idempotency key")
	}
	if h.Verifier == nil {
		return intents.Ambiguous("no collection verifier wired; exact receipt required")
	}
	method, err := h.DB.Gen(ctx).GetPaymentMethodByID(ctx, p.PaymentMethodID)
	if err != nil {
		return intents.Ambiguous("load payment method: " + err.Error())
	}
	res, err := h.Verifier.VerifyCollectionCharge(ctx, method, intent.ID.String())
	if err != nil {
		return intents.Ambiguous("provider read failed: " + err.Error())
	}
	if !res.Supported {
		return intents.Ambiguous(fmt.Sprintf("rail %q has no provider read; exact receipt required", p.Rail))
	}
	if !res.Settled {
		return intents.Ambiguous("submitted collection has no exact provider receipt; no automatic resend")
	}
	if strings.TrimSpace(res.TransactionID) == "" {
		return intents.Ambiguous("positive provider result has no transaction receipt")
	}
	return h.finalizeSettle(ctx, intent, p, p.Rail, res.TransactionID, res.ExternalInvoiceID, true)
}

// Resolve applies operator evidence: an exact provider object confirmed as
// this operation's settled charge, or provider-confirmed non-execution. It
// never re-sends.
func (h *InvoiceCollectionHandler) Resolve(ctx context.Context, intent gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	p, err := decodeInvoiceCollectionPayload(intent)
	if err != nil {
		return intents.Outcome{}, err
	}
	if resolution.Step != "" {
		return intents.Outcome{}, fmt.Errorf("%w: an invoice collection has no steps", intents.ErrResolutionInvalid)
	}
	if receipt := intents.EvidenceString(intent, collectionEvidenceTransactionID); receipt != "" {
		return intents.Outcome{}, intents.RejectResolution("operation already holds receipt %s; its verifier completes settlement", receipt)
	}
	if intents.EvidenceString(intent, collectionEvidenceFailureCode) != "" || intents.EvidenceString(intent, collectionEvidenceNotExecuted) != "" {
		return intents.Outcome{}, intents.RejectResolution("operation already holds a definitive provider answer; its verifier completes it")
	}
	if h.Verifier == nil {
		return intents.Outcome{}, errors.New("no collection verifier wired")
	}
	method, err := h.DB.Gen(ctx).GetPaymentMethodByID(ctx, p.PaymentMethodID)
	if err != nil {
		return intents.Outcome{}, fmt.Errorf("load payment method: %w", err)
	}
	expect := CollectionReceiptExpectation{OperationKey: intent.ID.String(), Amount: p.AmountMinor, Currency: p.Currency}
	if resolution.NotExecuted {
		if err := h.Verifier.ConfirmCollectionNotExecuted(ctx, method, expect); err != nil {
			return intents.Outcome{}, intents.RejectResolution("%v", err)
		}
		return h.finalizeNotExecuted(ctx, intent, p), nil
	}
	res, err := h.Verifier.ConfirmCollectionReceipt(ctx, method, resolution.ProviderReference, expect)
	if err != nil {
		return intents.Outcome{}, intents.RejectResolution("%v", err)
	}
	if !res.Settled || strings.TrimSpace(res.TransactionID) == "" {
		return intents.Outcome{}, intents.RejectResolution("provider object %s is not a settled charge for this operation", resolution.ProviderReference)
	}
	return h.finalizeSettle(ctx, intent, p, p.Rail, res.TransactionID, res.ExternalInvoiceID, true), nil
}

// ResolveUnsent releases a pending operation that never reached the provider:
// the write-ahead fence is absent, so nothing can have been charged. The
// attempt fails without a decline and the invoice becomes due again. An
// operation carrying the fence belongs to its verifier.
func (h *InvoiceCollectionHandler) ResolveUnsent(ctx context.Context, intent gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	p, err := decodeInvoiceCollectionPayload(intent)
	if err != nil {
		return intents.Outcome{}, err
	}
	if !resolution.NotExecuted || resolution.Step != "" {
		return intents.Outcome{}, fmt.Errorf("%w: a never-submitted collection accepts only --not-executed", intents.ErrResolutionInvalid)
	}
	if intents.EvidenceString(intent, collectionEvidenceSubmittedAt) != "" {
		return intents.Outcome{}, intents.RejectResolution("operation %s crossed its submission fence; only its verifier or an unknown-state resolution can close it", intent.ID)
	}
	return h.finalizeNotExecuted(ctx, intent, p), nil
}

// finalizeFromEvidence retries local effects for an answer the operation
// already holds (a receipt, a parsed refusal, confirmed non-execution).
func (h *InvoiceCollectionHandler) finalizeFromEvidence(ctx context.Context, intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload) (intents.Outcome, bool) {
	rail := intents.EvidenceString(intent, collectionEvidenceRail)
	if rail == "" {
		rail = p.Rail
	}
	if receipt := intents.EvidenceString(intent, collectionEvidenceTransactionID); receipt != "" {
		return h.finalizeSettle(ctx, intent, p, rail, receipt, intents.EvidenceString(intent, collectionEvidenceExternalID), true), true
	}
	if intents.EvidenceString(intent, collectionEvidenceNotExecuted) != "" {
		return h.finalizeNotExecuted(ctx, intent, p), true
	}
	if code := intents.EvidenceString(intent, collectionEvidenceFailureCode); code != "" {
		return h.finalizeRefusal(ctx, intent, p, rail, code, intents.EvidenceString(intent, collectionEvidenceFailureMsg), ""), true
	}
	return intents.Outcome{}, false
}

// finalizeSettle records one confirmed charge exactly once: invoice snapshot
// applied, arrears liability settled on the ledger (deduped on the operation
// key), attempt settled, invoice released — all or nothing. A local failure
// retains the receipt on the operation for the verifier and keeps the
// invoice pointed at it.
func (h *InvoiceCollectionHandler) finalizeSettle(ctx context.Context, intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload, rail, transactionID, externalInvoiceID string, verified bool) intents.Outcome {
	transactionID = strings.TrimSpace(transactionID)
	evidence := map[string]any{collectionEvidenceTransactionID: transactionID, collectionEvidenceRail: rail}
	if ext := strings.TrimSpace(externalInvoiceID); ext != "" {
		evidence[collectionEvidenceExternalID] = ext
	}
	now := h.now()
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		attempt, err := q.GetInvoicePaymentAttempt(ctx, gen.GetInvoicePaymentAttemptParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, AttemptID: p.AttemptID})
		if err != nil {
			return fmt.Errorf("load attempt: %w", err)
		}
		switch attempt.Status {
		case "settled":
			return nil
		case "failed":
			return fmt.Errorf("attempt %s already failed; a confirmed charge %s needs repair", attempt.ID, transactionID)
		}
		applied, err := q.ApplyInvoicePaymentSnapshot(ctx, gen.ApplyInvoicePaymentSnapshotParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, Snapshot: p.Amount, Now: now})
		if err != nil {
			return err
		}
		if applied != 1 {
			// The invoice changed under its own live operation (only raw
			// surgery can do that). Fail closed: nothing is written, the
			// receipt stays on the operation and the pointer keeps every
			// further collection off this invoice until an operator repairs it.
			return fmt.Errorf("invoice %s no longer accepts the frozen snapshot %d; confirmed charge %s needs repair", p.InvoiceID, p.Amount, transactionID)
		}
		if ext := optionalString(externalInvoiceID); ext != nil {
			if _, err := q.SetInvoiceExternalID(ctx, gen.SetInvoiceExternalIDParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, ExternalInvoiceID: ext, Now: now}); err != nil {
				return err
			}
		}
		currency := normalizeCurrency(p.Currency)
		ml := ledger.New(q, intent.MerchantID)
		coord := ledger.Coord{Operation: ledger.OpInvoicePayment, Source: "invoice_charge", SourceID: intent.IdempotencyKey}
		transfer, err := q.GetLedgerTransferByCoords(ctx, gen.GetLedgerTransferByCoordsParams{
			MerchantID: intent.MerchantID, CustomerID: p.CustomerID, Currency: currency,
			TransferType: "owed_payment", Operation: string(coord.Operation), Source: coord.Source, SourceID: coord.SourceID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			invoiceID := p.InvoiceID
			transfer, err = ml.PayOwed(ctx, p.CustomerID, currency, p.Amount, coord, &invoiceID)
		}
		if err != nil {
			return err
		}
		settled, err := q.SettleClaimedInvoicePaymentAttempt(ctx, gen.SettleClaimedInvoicePaymentAttemptParams{
			MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, AttemptID: p.AttemptID,
			LedgerTransferID: &transfer.ID, Rail: optionalRail(rail), RailPaymentID: optionalString(transactionID), Now: now,
		})
		if err != nil {
			return err
		}
		if settled != 1 {
			return errors.New("settle attempt: claim lost")
		}
		released, err := q.ReleaseInvoiceCollection(ctx, gen.ReleaseInvoiceCollectionParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, IntentID: intent.ID, Now: now})
		if err != nil {
			return err
		}
		if released != 1 {
			return fmt.Errorf("invoice %s no longer names this operation; confirmed charge %s needs repair", p.InvoiceID, transactionID)
		}
		return nil
	})
	if err != nil {
		return intents.AmbiguousWithEvidence("collection charged, but local settlement failed: "+err.Error(), evidence)
	}
	if verified {
		evidence["verified_existing"] = true
	}
	return intents.Succeeded(evidence)
}

// finalizeRefusal records a definitive provider refusal: attempt failed,
// invoice dunned by the or#870 decline doctrine on its own billing cycle,
// invoice released. A local failure retains the refusal for the verifier.
func (h *InvoiceCollectionHandler) finalizeRefusal(ctx context.Context, intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload, rail, failureCode, failureMessage, transactionID string) intents.Outcome {
	if strings.TrimSpace(failureCode) == "" {
		failureCode = "declined"
	}
	evidence := map[string]any{collectionEvidenceDeclined: true, collectionEvidenceFailureCode: failureCode, collectionEvidenceRail: rail}
	if failureMessage != "" {
		evidence[collectionEvidenceFailureMsg] = failureMessage
	}
	now := h.now()
	var action collection.Action
	var invoice *models.Invoice
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		attempt, err := q.GetInvoicePaymentAttempt(ctx, gen.GetInvoicePaymentAttemptParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, AttemptID: p.AttemptID})
		if err != nil {
			return fmt.Errorf("load attempt: %w", err)
		}
		switch attempt.Status {
		case "failed":
			return nil
		case "settled":
			return fmt.Errorf("attempt %s already settled; refusal contradicts a recorded charge", attempt.ID)
		}
		row, err := q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, ID: p.InvoiceID})
		if err != nil {
			return fmt.Errorf("lock invoice: %w", err)
		}
		invoice, err = invoiceFromGen(row)
		if err != nil {
			return err
		}
		code := failureCode
		action = collection.FailureAction(invoiceCycleHours(invoice), rail, &code, int(invoice.CollectionFailureCount), invoice.CollectionFailedAt, now)
		failureReason := payments.NormalizeFailureReason(rail, failureCode)
		failed, err := q.FailClaimedInvoicePaymentAttempt(ctx, gen.FailClaimedInvoicePaymentAttemptParams{
			MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, AttemptID: p.AttemptID,
			Rail: optionalRail(rail), RailPaymentID: optionalString(transactionID), FailureCode: &code, FailureReason: &failureReason,
			FailureMessage: optionalString(failureMessage), Now: now,
		})
		if err != nil {
			return err
		}
		if failed != 1 {
			return errors.New("fail attempt: claim lost")
		}
		updated, err := q.RecordInvoiceCollectionFailure(ctx, gen.RecordInvoiceCollectionFailureParams{
			MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, IntentID: intent.ID,
			Terminal: action.Terminal, NextAttemptAt: action.NextAttemptAt, FailureCode: &code, FailureMessage: optionalString(failureMessage), Now: now,
		})
		if err != nil {
			return err
		}
		if updated != 1 {
			return errors.New("record collection failure: invoice no longer names this operation")
		}
		return nil
	})
	if err != nil {
		return intents.AmbiguousWithEvidence("collection refused, but local record failed: "+err.Error(), evidence)
	}
	if invoice != nil {
		logInvoiceDeclineDecision(ctx, p.InvoiceID, rail, &failureCode, action)
		h.notifyInvoiceCollectionOutcome(ctx, invoice, action, failureCode, now)
	}
	return intents.TerminalWithEvidence("collection refused: "+failureCode, evidence)
}

// finalizeNotExecuted records provider-confirmed non-execution: the attempt
// fails without a decline and the invoice is due again immediately.
func (h *InvoiceCollectionHandler) finalizeNotExecuted(ctx context.Context, intent gen.OpenrailsRailIntent, p InvoiceCollectionPayload) intents.Outcome {
	evidence := map[string]any{collectionEvidenceNotExecuted: true, collectionEvidenceDeclined: false}
	now := h.now()
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		attempt, err := q.GetInvoicePaymentAttempt(ctx, gen.GetInvoicePaymentAttemptParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, AttemptID: p.AttemptID})
		if err != nil {
			return fmt.Errorf("load attempt: %w", err)
		}
		switch attempt.Status {
		case "failed":
			return nil
		case "settled":
			return fmt.Errorf("attempt %s already settled; non-execution contradicts a recorded charge", attempt.ID)
		}
		code, reason := "not_executed", "provider confirmed the collection was not executed"
		if _, err := q.FailClaimedInvoicePaymentAttempt(ctx, gen.FailClaimedInvoicePaymentAttemptParams{
			MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, AttemptID: p.AttemptID,
			Rail: optionalRail(p.Rail), FailureCode: &code, FailureReason: &code, FailureMessage: &reason, Now: now,
		}); err != nil {
			return err
		}
		_, err = q.ReleaseInvoiceCollection(ctx, gen.ReleaseInvoiceCollectionParams{MerchantID: intent.MerchantID, CustomerID: p.CustomerID, InvoiceID: p.InvoiceID, IntentID: intent.ID, NextAttemptAt: &now, Now: now})
		return err
	})
	if err != nil {
		return intents.AmbiguousWithEvidence("non-execution confirmed, but local release failed: "+err.Error(), evidence)
	}
	return intents.TerminalWithEvidence("provider confirmed the collection was not executed", evidence)
}

// invoiceCycleHours is the invoice's own billing cycle for the dunning
// schedule: the statement window, floored at the shortest retriable cycle (a
// threshold statement can cover an hour and must not be written off on its
// first decline). 0 = unknown, handled as monthly by the schedule.
func invoiceCycleHours(invoice *models.Invoice) int {
	if invoice == nil {
		return 0
	}
	cycleHours := collection.CycleHoursBetween(invoice.PeriodFrom, invoice.PeriodTo)
	if cycleHours > 0 && cycleHours < collection.MinRetryCycleHours {
		return collection.MinRetryCycleHours
	}
	return cycleHours
}

// logInvoiceDeclineDecision names the bucket and its invoice-shaped
// consequence on every refusal. Nothing here cancels anything.
func logInvoiceDeclineDecision(ctx context.Context, invoiceID uuid.UUID, rail string, failureCode *string, action collection.Action) {
	collection.AlertUnmappedDecline(ctx, action.Decline)
	entry := log.WithContext(ctx).WithFields(log.Fields{
		"invoice_id": invoiceID, "rail": rail, "failure_code": derefStr(failureCode),
		"decline_outcome": action.Outcome.String(), "decline_coverage": action.Decline.Coverage.String(),
	})
	switch {
	case action.AwaitingPaymentMethod():
		entry.Warn("invoice collection: customer's card needs fixing (or#870 bucket 2); charging STOPS, the invoice stays OPEN")
	case action.Terminal && !action.ScheduleExhausted():
		entry.Error("invoice collection: non-recoverable decline (or#870 bucket 3); the invoice is uncollectible")
	case action.Terminal:
		entry.Error("invoice collection: the retry schedule is exhausted (or#870 bucket 1); the invoice is uncollectible")
	default:
		entry.WithField("next_attempt_at", action.NextAttemptAt).Warn("invoice collection: retryable decline (or#870 bucket 1); will retry on the invoice's own billing cycle")
	}
}

// notifyInvoiceCollectionOutcome is the or#870 notification ladder shaped for
// an invoice: one rung per bucket, never an access effect. Notification
// failure never fails collection.
func (h *InvoiceCollectionHandler) notifyInvoiceCollectionOutcome(ctx context.Context, invoice *models.Invoice, action collection.Action, failureCode string, now time.Time) {
	amountDue := invoice.AmountDue
	data := openrails.NotificationData{
		InvoiceID: invoice.ID, Currency: invoice.Currency, AmountDue: &amountDue,
		FailureCode: failureCode, DeclineOutcome: action.Outcome.String(),
	}
	eventType := models.NotificationPaymentMethodFailed
	switch {
	case action.AwaitingPaymentMethod():
		eventType = models.NotificationPaymentMethodUpdateRequired
	case action.Terminal:
		eventType = models.NotificationInvoiceCollectionStopped
		reason := "non_recoverable"
		if action.ScheduleExhausted() {
			reason = "schedule_exhausted"
		}
		data.Reason = reason
	case action.NextAttemptAt != nil:
		next := action.NextAttemptAt.UTC()
		data.NextAttemptAt = &next
	}
	notification := &models.NotificationQueue{ID: uuidutil.NewV7(), CustomerID: invoice.CustomerID, EventType: eventType, Data: data, CreatedAt: now}
	if err := subscriptions.NewNotificationQueueRepo(h.DB).Create(ctx, notification); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{"invoice_id": invoice.ID, "customer_id": invoice.CustomerID, "event_type": eventType}).
			Error("failed to queue invoice collection notification")
	}
}
