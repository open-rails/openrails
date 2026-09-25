package money

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

var errEngineObligationChanged = errors.New("accepted engine obligation changed before submission")

// SubscriptionCollectionHandler uses the collected-payment custody protocol
// and shared renewal writer for qualified engine card obligations.
type SubscriptionCollectionHandler struct {
	DB       *db.DB
	Resolver CollectionPlane
	Config   *config.Config
	Clock    clockwork.Clock
}

func NewSubscriptionCollectionHandler(d *db.DB, resolver CollectionPlane, cfg *config.Config, clock clockwork.Clock) *SubscriptionCollectionHandler {
	return &SubscriptionCollectionHandler{DB: d, Resolver: resolver, Config: cfg, Clock: timeutil.FirstClock(clock)}
}
func (*SubscriptionCollectionHandler) Type() string { return subscriptions.TypeSubscriptionCollection }
func (*SubscriptionCollectionHandler) Backoff(n int32) time.Duration {
	return intents.DefaultBackoff.Delay(n)
}
func (*SubscriptionCollectionHandler) PrunePolicy() (bool, bool)    { return true, true }
func (*SubscriptionCollectionHandler) CommitsTerminalOutcome() bool { return true }
func (*SubscriptionCollectionHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *SubscriptionCollectionHandler) now() time.Time { return h.Clock.Now().UTC() }

func (h *SubscriptionCollectionHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	current, err := intents.NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	in = current
	p, err := subscriptions.DecodeSubscriptionCollectionPayload(in)
	if err != nil {
		return intents.Parked(err.Error())
	}
	if outcome, done := h.completeEvidence(ctx, in, p); done {
		return outcome
	}
	if intents.EvidenceString(in, "submitted_at") != "" {
		if in.Rail == "stripe" {
			return h.executeStripeEngineDecline(ctx, in)
		}
		return h.resendLostNMISubmission(ctx, in, p)
	}
	if reason := h.submissionHeld(in); reason != "" {
		return intents.Parked(reason)
	}
	if outcome, done := h.obligationPaid(ctx, in, p); done {
		return outcome
	}
	method, _, _, err := h.validateAndFence(ctx, in, p, nil)
	if err != nil {
		if errors.Is(err, errEngineObligationChanged) || errors.Is(err, charge.ErrInstrumentChanged) {
			return h.completeNotExecuted(ctx, in, p, "instrument_changed", err.Error())
		}
		return intents.Parked(err.Error())
	}
	if in.Rail == "stripe" {
		return h.executeStripeEngine(ctx, in, p)
	}
	charger, err := prepareEngineNMICharge(ctx, h.Resolver, method, p.HyperSwitch)
	if err != nil {
		return intents.Parked("arm accepted recurring charge: " + err.Error())
	}
	_, proof, first, err := h.validateAndFence(ctx, in, p, h.firstSubmission(in))
	if err != nil {
		if errors.Is(err, errEngineObligationChanged) || errors.Is(err, charge.ErrInstrumentChanged) {
			return h.completeNotExecuted(ctx, in, p, "instrument_changed", err.Error())
		}
		return intents.Parked("fence engine submission: " + err.Error())
	}
	if !first {
		return h.Verify(ctx, in)
	}
	if err := h.hit(ctx, in, failpoint.AfterFence); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.dispatchNMI(ctx, in, p, charger, proof, 0)
}

// obligationPaid reads the obligation's shared order before a later attempt
// is fenced: an earlier attempt's charge, found late, pays the period and
// nothing is sent.
func (h *SubscriptionCollectionHandler) obligationPaid(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload) (intents.Outcome, bool) {
	if in.Rail == "stripe" || p.Attempt == 0 || p.Instrument.CustodianHeld() {
		return intents.Outcome{}, false
	}
	receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, h.Resolver, "")
	if err != nil {
		return h.unresolved(ctx, in, p, "the obligation's order cannot be read before a new attempt: "+err.Error()), true
	}
	if !found {
		return intents.Outcome{}, false
	}
	return h.completePaid(ctx, in, p, receipt), true
}

// hit runs the named failpoint for this operation.
func (h *SubscriptionCollectionHandler) hit(ctx context.Context, in gen.OpenrailsRailIntent, point failpoint.Point) error {
	site := failpoint.Site{Point: point, Kind: in.IntentType, Operation: in.ID}
	if in.SubscriptionID != nil {
		site.Subscription = *in.SubscriptionID
	}
	return failpoint.Hit(ctx, site)
}

func (h *SubscriptionCollectionHandler) submissionHeld(in gen.OpenrailsRailIntent) string {
	if h.Config == nil {
		return "engine execution mode is not configured"
	}
	if h.Config.EngineAdmissionHold {
		return "new engine payment submission is held"
	}
	if blocked, reason := intents.GateExecution(h.Config, intents.Origin(in.Origin)); blocked {
		return reason
	}
	if h.Resolver == nil {
		return "engine collection resolver is unavailable"
	}
	return ""
}

// dispatchNMI sends the accepted charge under its order reference. Only the
// writer of a fresh submission or resend fence calls it.
func (h *SubscriptionCollectionHandler) dispatchNMI(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, charger recurringNMICharger, proof intents.CollectionNonexecutionProof, dupSeconds int) intents.Outcome {
	chargeContext := charge.RecurringMIT(p.Instrument.StoredCredentialRecurringRef)
	execute := charger.ChargeRecurringMIT
	if p.Initiator == charge.InitiatorCustomer {
		chargeContext = charge.RecurringReuse(p.Instrument.StoredCredentialRecurringRef)
		execute = charger.ChargeInitialRecurring
	}
	if err := h.hit(ctx, in, failpoint.BeforeProvider); err != nil {
		return intents.Ambiguous(err.Error())
	}
	if err := intents.NewStore(h.DB).RequireClaim(ctx, in.ID, h.now()); err != nil {
		return intents.Ambiguous("nothing sent: " + err.Error())
	}
	result, refusal, err := execute(ctx, charge.Request{
		Instrument:  charge.Instrument{PaymentMethodID: p.PaymentMethodID, Rail: "nmi", CustomerRef: p.Instrument.RailCustomerRef, MethodRef: p.Instrument.RailMethodRef},
		AmountMinor: p.AmountMinor, Currency: p.Renewal.Currency, OrderRef: p.OrderReference, Description: "OpenRails subscription renewal", Context: chargeContext,
		DupSeconds: dupSeconds,
	})
	if hitErr := h.hit(ctx, in, failpoint.AfterProvider); hitErr != nil {
		return intents.Ambiguous(hitErr.Error())
	}
	if errors.Is(err, charge.ErrNotDispatched) {
		return h.completeNotExecuted(ctx, in, p, "not_dispatched", charge.ErrNotDispatched.Error(), proof)
	}
	if errors.Is(err, nmi.ErrDuplicateTransaction) {
		if err := intents.NewStore(h.DB).RecordDuplicateRefusal(ctx, in, h.now()); err != nil {
			return intents.Ambiguous("record duplicate refusal: " + err.Error())
		}
		return intents.Ambiguous("NMI refused the renewal as a duplicate of a recent charge; verifying whether that charge paid this period")
	}
	if err != nil {
		return intents.Ambiguous("engine submission requires exact receipt recovery: " + err.Error())
	}
	if refusal != nil {
		if err := intents.NewStore(h.DB).RetainRecurringDecline(ctx, in, refusal.ResponseCode, result.TransactionID); err != nil {
			return intents.Ambiguous("retain engine refusal: " + err.Error())
		}
		return h.Verify(ctx, in)
	}
	if result.Declined || result.TransactionID == "" {
		return intents.Ambiguous("engine response has no typed refusal or payment reference")
	}
	if err := intents.NewStore(h.DB).RetainCollectionCandidate(ctx, in, intents.CollectionCandidate{TransactionID: result.TransactionID}); err != nil {
		return intents.Ambiguous("retain engine candidate: " + err.Error())
	}
	return h.Verify(ctx, in)
}

// submissionFence takes a write-once submission fence inside the validation
// transaction: the original submission or one armed resend.
type submissionFence func(context.Context, *intents.Store) (intents.CollectionNonexecutionProof, bool, error)

func (h *SubscriptionCollectionHandler) firstSubmission(in gen.OpenrailsRailIntent) submissionFence {
	return func(ctx context.Context, s *intents.Store) (intents.CollectionNonexecutionProof, bool, error) {
		return s.BeginCollectedPayment(ctx, in, h.now())
	}
}

func (h *SubscriptionCollectionHandler) resendSubmission(in gen.OpenrailsRailIntent, attempt int) submissionFence {
	return func(ctx context.Context, s *intents.Store) (intents.CollectionNonexecutionProof, bool, error) {
		return s.BeginLostSubmissionResend(ctx, in, attempt, h.now())
	}
}

// Local lock order matches admission/deletion: customer, subscription, method,
// then operation. No provider request runs while these locks are held.
func (h *SubscriptionCollectionHandler) validateAndFence(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, fence submissionFence) (gen.OpenrailsPaymentMethod, intents.CollectionNonexecutionProof, bool, error) {
	var method gen.OpenrailsPaymentMethod
	if h.now().Before(p.AcceptedAt) {
		return method, intents.CollectionNonexecutionProof{}, false, errors.New("engine admission time has not arrived")
	}
	var proof intents.CollectionNonexecutionProof
	var first bool
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.Renewal.CustomerID}); err != nil {
			return err
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.Renewal.SubscriptionID)
		if err != nil {
			return err
		}
		failures := 0
		if sub.RetryAttempts != nil {
			failures = *sub.RetryAttempts
		}
		if sub.CollectionPolicy != models.CollectionPolicyEngine || string(sub.Rail) != in.Rail || sub.RailSubscriptionID != "" || sub.CustomerID != p.Renewal.CustomerID || sub.PspID != p.Instrument.PSPID || sub.PaymentMethodID == nil || *sub.PaymentMethodID != p.PaymentMethodID || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(p.PreviousPeriodEnd) || sub.PriceID != p.Renewal.FromPriceID || sub.ProductID != p.Renewal.FromProductID || (sub.Status != models.StatusActive && sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod) || sub.CancelledAt != nil || sub.DeletionScheduledAt != nil || failures != p.FailureCount {
			return errEngineObligationChanged
		}
		if p.Instrument.CustodianID != nil {
			handle := paymentmethods.CustodianHandle{Custodian: *p.Instrument.CustodianID, Method: p.Instrument.RailMethodRef}
			if err := paymentmethods.LockCustodianHandles(ctx, q, in.MerchantID, handle); err != nil {
				return err
			}
			if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, in.MerchantID, handle); err != nil {
				return errors.Join(charge.ErrInstrumentChanged, err)
			}
		}
		method, err = q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != p.Renewal.CustomerID || method.ParkReason != "" || method.ChargeVia != "pan_proxy" {
			return charge.ErrInstrumentChanged
		}
		if err := p.Instrument.Matches(method, charge.AgreementRecurring); err != nil {
			return err
		}
		binding, err := engineCollectionBinding(ctx, q, method, p.HyperSwitch.APIBaseURL)
		if err != nil {
			return err
		}
		if binding != p.HyperSwitch {
			return charge.ErrInstrumentChanged
		}
		if fence != nil {
			proof, first, err = fence(ctx, intents.NewStore(d))
			return err
		}
		return nil
	})
	return method, proof, first, err
}

func (h *SubscriptionCollectionHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	current, err := intents.NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	in = current
	p, err := subscriptions.DecodeSubscriptionCollectionPayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if outcome, done := h.completeEvidence(ctx, in, p); done {
		return outcome
	}
	if intents.EvidenceString(in, "submitted_at") == "" {
		return intents.Retryable("unsubmitted payment awaits gated execution")
	}
	reference := ""
	if candidate, found, err := intents.LoadCollectionCandidate(in); err != nil {
		return intents.Ambiguous(err.Error())
	} else if found {
		reference = candidate.TransactionID
	}
	if in.Rail == "stripe" {
		return h.verifyStripeEngine(ctx, in, p, reference)
	}
	receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, h.Resolver, reference)
	if err != nil {
		return h.unresolved(ctx, in, p, "engine receipt did not qualify: "+err.Error())
	}
	if found {
		return h.completePaid(ctx, in, p, receipt)
	}
	if p.Instrument.CustodianHeld() {
		return h.unresolved(ctx, in, p, "custodian-held engine charge has no exact receipt")
	}
	attempts, err := intents.ReadNMIOrderAttempts(ctx, in, h.Resolver)
	if err != nil {
		return h.unresolved(ctx, in, p, "order read is inconclusive: "+err.Error())
	}
	if attempts.Declined {
		if err := intents.NewStore(h.DB).RetainRecurringDecline(ctx, in, attempts.DeclineCode, attempts.DeclineTransactionID); err != nil {
			return h.unresolved(ctx, in, p, "retain the order's decline: "+err.Error())
		}
		return h.Verify(ctx, in)
	}
	if attempts.Transactions > 0 {
		return h.unresolved(ctx, in, p, "the order holds transactions that are not one approved or declined sale")
	}
	return h.lostSubmission(ctx, in, p)
}
func (h *SubscriptionCollectionHandler) completeEvidence(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload) (intents.Outcome, bool) {
	if receipt, found, err := intents.LoadCollectedReceipt(in); err != nil {
		return intents.Ambiguous(err.Error()), true
	} else if found {
		return h.completePaid(ctx, in, p, receipt), true
	}
	if code, reference, found, err := intents.LoadStripeRecurringDecline(in); err != nil {
		return intents.Ambiguous(err.Error()), true
	} else if found {
		outcome := intents.TerminalWithEvidence("engine renewal declined", map[string]any{"declined": true, "failure_code": code, "stripe_payment_intent_id": reference})
		return h.completeDecline(ctx, in, p, code, outcome), true
	}
	if code, reference, found, err := intents.LoadRecurringDecline(in); err != nil {
		return intents.Ambiguous(err.Error()), true
	} else if found {
		return h.completeDeclined(ctx, in, p, code, reference), true
	}
	if proof, found, err := intents.LoadCollectionNonexecution(in); err != nil {
		return intents.Ambiguous(err.Error()), true
	} else if found {
		code, reason := proof.Refusal()
		return h.completeNotExecuted(ctx, in, p, code, reason), true
	}
	return intents.Outcome{}, false
}
func (h *SubscriptionCollectionHandler) lifecycle(d *db.DB) *subscriptions.SubscriptionLifecycleService {
	lc := subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, payments.NewPaymentService(d, h.Clock), h.Clock)
	lc.SetConfig(h.Config)
	return lc
}
func (h *SubscriptionCollectionHandler) completion(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, outcome intents.Outcome, apply func(context.Context, *db.DB, *models.Subscription) error) intents.Outcome {
	if err := h.hit(ctx, in, failpoint.BeforeComplete); err != nil {
		return intents.Ambiguous(err.Error())
	}
	ctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.Renewal.CustomerID}); err != nil {
			return err
		}
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.Renewal.SubscriptionID)
		if err != nil {
			return err
		}
		current, err := intents.NewStore(d).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		if current.Status != intents.StatusSucceeded && current.Status != intents.StatusFailedTerminal {
			if err := apply(ctx, d, sub); err != nil {
				return err
			}
		}
		if _, err := d.Gen(ctx).ResolveStandingFinding(ctx, gen.ResolveStandingFindingParams{MerchantID: in.MerchantID, FindingType: FindingSubmissionUnresolved, SubjectKey: in.ID.String()}); err != nil {
			return err
		}
		return intents.NewStore(d).CompleteSubscriptionCollection(ctx, in, outcome, h.now())
	})
	if err != nil {
		return intents.Ambiguous("engine local completion resumes from retained custody: " + err.Error())
	}
	return outcome
}
func (h *SubscriptionCollectionHandler) completePaid(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, receipt intents.CollectedReceipt) intents.Outcome {
	retained, err := intents.NewStore(h.DB).RetainCollectedReceipt(ctx, in, receipt)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	outcome := intents.Succeeded(map[string]any{"transaction_id": retained.TransactionID(), "rail": in.Rail, "verified_existing": true})
	return h.completion(ctx, in, p, outcome, func(ctx context.Context, d *db.DB, sub *models.Subscription) error {
		params := &subscriptions.RenewMembershipParams{Prepared: &p.Renewal, PreviousPeriodEnd: &p.PreviousPeriodEnd, PaymentCustodian: p.Instrument.Custodian, Rail: models.Rail(in.Rail), TransactionID: retained.TransactionID(), Amount: p.Renewal.Amount, AmountProvided: true, Currency: p.Renewal.Currency}
		current := sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.Equal(p.PreviousPeriodEnd) && sub.PriceID == p.Renewal.FromPriceID && sub.ProductID == p.Renewal.FromProductID
		replay := sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.Equal(p.Renewal.PeriodEnd) && sub.PriceID == p.Renewal.PriceID && sub.ProductID == p.Renewal.ProductID
		if reversal := retained.ReversalKind(); reversal != "" {
			params.PaymentMetadata = map[string]any{"refund_review": "confirmed charge on a cancelled subscription"}
			if err := h.lifecycle(d).RecordConfirmedChargeWithoutRenewal(ctx, params); err != nil {
				return err
			}
			if sub.Status != models.StatusCancelled {
				kind := models.CancelTypeMerchant
				if reversal == "dispute" {
					kind = models.CancelTypeChargeback
				}
				_, err := h.lifecycle(d).CancelMembershipTx(ctx, d, &subscriptions.CancelMembershipParams{SubscriptionID: &sub.ID, CancelType: kind, RevokeAccess: true})
				return err
			}
			return nil
		}
		if sub.Status == models.StatusCancelled || (!current && !replay) {
			params.PaymentMetadata = map[string]any{"refund_review": "accepted engine charge completed after lifecycle changed"}
			return h.lifecycle(d).RecordConfirmedChargeWithoutRenewal(ctx, params)
		}
		return h.lifecycle(d).RenewMembership(ctx, params)
	})
}
func (h *SubscriptionCollectionHandler) completeDeclined(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, response int, reference string) intents.Outcome {
	outcome := intents.TerminalWithEvidence("engine renewal declined", map[string]any{"declined": true, "response_code": response})
	return h.completeDecline(ctx, in, p, strconv.Itoa(response), outcome)
}
func (h *SubscriptionCollectionHandler) completeDecline(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, code string, outcome intents.Outcome) intents.Outcome {
	return h.completion(ctx, in, p, outcome, func(ctx context.Context, d *db.DB, sub *models.Subscription) error {
		reason := payments.NormalizeFailureReason(in.Rail, code)
		kind := payments.AttemptRenewal
		failed := &models.Payment{ID: uuid.NewSHA1(in.ID, []byte("decline")), CustomerID: p.Renewal.CustomerID, PriceID: p.Renewal.PriceID, SubscriptionID: &p.Renewal.SubscriptionID, Rail: models.Rail(in.Rail), PspID: &p.Instrument.PSPID, TransactionID: "engine_declined:" + in.ID.String(), Amount: p.Renewal.Amount, ListAmount: p.Renewal.Amount, Currency: p.Renewal.Currency, Status: payments.PaymentStatusFailedValue, FailureCode: &code, FailureReason: &reason, AttemptKind: &kind, MoneyMovement: models.MoneyMovementNone, EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(p.Renewal.Entitlements), PurchasedAt: h.now(), CreatedAt: h.now()}
		if _, err := payments.NewPaymentService(d, h.Clock).CreateIfNotExists(ctx, failed); err != nil {
			return err
		}
		failures := 0
		if sub.RetryAttempts != nil {
			failures = *sub.RetryAttempts
		}
		if (sub.Status == models.StatusActive || sub.Status == models.StatusPastDue || sub.Status == models.StatusAwaitingMethod) && sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.Equal(p.PreviousPeriodEnd) && failures == p.FailureCount {
			verdict := collection.ClassifyDeclineDetail(in.Rail, code)
			if in.Rail == "stripe" && code == "canceled" {
				verdict.Outcome = collection.DeclineFixPaymentMethod
			}
			certainty := ""
			if verdict.Outcome == collection.DeclineNonRecoverable {
				certainty = collection.CertaintyNonRetryableDecline
			}
			blocked := ""
			if gate := destructive.New(d).Check(ctx, in.MerchantID); !gate.Allowed {
				blocked = gate.Reason
			}
			return h.lifecycle(d).FailMembership(ctx, &subscriptions.FailMembershipParams{Prepared: &p.Renewal, Rail: models.Rail(in.Rail), SubscriptionID: &p.Renewal.SubscriptionID, FailureCode: &code, FailureReason: &reason, Decline: verdict.Outcome, AttemptRecorded: true, TerminalCertainty: certainty, TerminalBlocked: blocked})
		}
		return nil
	})
}
func (h *SubscriptionCollectionHandler) completeNotExecuted(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.SubscriptionCollectionPayload, code, reason string, proof ...intents.CollectionNonexecutionProof) intents.Outcome {
	if len(proof) > 1 {
		return intents.Ambiguous("invalid engine nonexecution capability")
	}
	if len(proof) == 1 {
		if err := intents.NewStore(h.DB).RetainCollectionNonexecution(ctx, in, proof[0], code, reason); err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	outcome := intents.TerminalWithEvidence(reason, map[string]any{"not_executed": true, "declined": false, "not_executed_code": code})
	return h.completion(ctx, in, p, outcome, func(context.Context, *db.DB, *models.Subscription) error { return nil })
}

var _ intents.Handler = (*SubscriptionCollectionHandler)(nil)
