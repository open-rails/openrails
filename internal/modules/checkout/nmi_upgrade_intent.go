package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const TypeNMIUpgrade = subscriptions.TypeNMIUpgrade

type nmiUpgradeStep struct {
	SubmittedAt time.Time         `json:"submitted_at"`
	Sale        *nmi.SaleResponse `json:"sale,omitempty"`
	Refusal     string            `json:"refusal,omitempty"`
	// RefusalStatus/RefusalCode classify a provider refusal for the route: a
	// card decline is 402 with its failure code, another rejection 400. An
	// operator-attested non-execution carries neither.
	RefusalCode   string `json:"refusal_code,omitempty"`
	RefusalStatus int    `json:"refusal_status,omitempty"`
	// Resolution records operator evidence that supplied this step's outcome.
	Resolution map[string]any `json:"resolution,omitempty"`
}

// refuse records a definitive provider refusal of the step.
func (s *nmiUpgradeStep) refuse(err error) {
	s.Refusal, s.RefusalStatus = err.Error(), http.StatusBadRequest
	var decline *nmi.CustomerVaultError
	if errors.As(err, &decline) && decline.ResponseCode >= 200 && decline.ResponseCode < 300 {
		s.RefusalStatus, s.RefusalCode = http.StatusPaymentRequired, nmidirect.FailureCode(decline)
	}
}

// refuseUnexecuted records that the provider holds no charge for the step, so
// nothing was charged and the customer may try again.
func (s *nmiUpgradeStep) refuseUnexecuted(code, reason string) {
	s.Refusal, s.RefusalStatus, s.RefusalCode = reason, http.StatusConflict, code
}

const (
	duplicateProrationRefusal = "the payment provider refused the charge as a duplicate of an identical charge just made on this card; nothing was charged, try again in a few minutes"
	absentProrationRefusal    = "the payment provider holds no charge for this tier change; nothing was charged, try again"
)

// ProrationUnresolvedFinding is a submitted tier-change charge whose outcome
// the provider's reads cannot settle. It names the operator resolve path and
// closes when the operation completes either way.
const ProrationUnresolvedFinding = "life.tier_change.proration_unresolved"

// nmiScheduleUpdate is the in-place change of the existing NMI schedule's
// amount. It is a set-to-value operation verified by readback, so retries are
// idempotent and never touch the schedule's next billing date.
type nmiScheduleUpdate struct {
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	Done      bool   `json:"done,omitempty"`
}

type nmiUpgradeProgress struct {
	Proration *nmiUpgradeStep    `json:"proration,omitempty"`
	Update    *nmiScheduleUpdate `json:"schedule_update,omitempty"`
}

func (p nmiUpgradeProgress) refused() *nmiUpgradeStep {
	if p.Proration != nil && p.Proration.Refusal != "" {
		return p.Proration
	}
	return nil
}

// ProviderUpdateStuckFinding is raised when a paid tier change cannot move the
// NMI schedule to the new amount; it closes once the update converges.
const ProviderUpdateStuckFinding = "life.tier_change.provider_update_stuck"

// nmiUpdateStuckAttempts is how many failed schedule updates raise the finding.
const nmiUpdateStuckAttempts = 3

// NMIUpgradeIntentHandler runs an in-place tier change of an NMI-billed
// subscription: the prorated charge (upgrade only), then the schedule amount
// update, then the local commit. Each step resumes from recorded progress.
type NMIUpgradeIntentHandler struct{ Checkout *CheckoutService }

func NewNMIUpgradeIntentHandler(s *CheckoutService) *NMIUpgradeIntentHandler {
	return &NMIUpgradeIntentHandler{Checkout: s}
}
func (*NMIUpgradeIntentHandler) Type() string { return TypeNMIUpgrade }
func (*NMIUpgradeIntentHandler) Backoff(attempts int32) time.Duration {
	return intents.DefaultBackoff.Delay(attempts)
}
func (*NMIUpgradeIntentHandler) PrunePolicy() (bool, bool)    { return true, true }
func (*NMIUpgradeIntentHandler) CommitsTerminalOutcome() bool { return true }
func (*NMIUpgradeIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *NMIUpgradeIntentHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	return h.advance(ctx, in, true)
}
func (h *NMIUpgradeIntentHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	return h.advance(ctx, in, false)
}

func (h *NMIUpgradeIntentHandler) advance(ctx context.Context, in gen.OpenrailsRailIntent, send bool) intents.Outcome {
	if h.Checkout == nil || h.Checkout.Lifecycle == nil {
		return intents.Parked("tier change lifecycle unavailable")
	}
	p, err := subscriptions.DecodeNMIUpgradePayload(in)
	if err != nil {
		return h.terminal(ctx, in, intents.Terminal("invalid tier change payload: "+err.Error()))
	}
	proration, err := moneyutil.NativeToRailMinorExact(p.Currency, p.ProrationAmount)
	if err != nil {
		return h.terminal(ctx, in, intents.Terminal(err.Error()))
	}
	database := h.Checkout.SubscriptionService.Database()
	store := intents.NewStore(database)
	current, err := store.Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous("read tier change progress: " + err.Error())
	}
	var progress nmiUpgradeProgress
	if len(current.ResultEvidence) > 0 {
		if err = json.Unmarshal(current.ResultEvidence, &progress); err != nil {
			return intents.Ambiguous("read tier change progress: " + err.Error())
		}
	}
	evidence := func() map[string]any {
		return map[string]any{"proration": progress.Proration, "schedule_update": progress.Update}
	}
	save := func(key string, step any) error {
		wctx, cancel := intents.LedgerWriteContext(ctx)
		defer cancel()
		return store.RecordProgress(wctx, in.ID, map[string]any{key: step})
	}
	receipt, receiptFound, err := intents.LoadCollectedReceipt(current)
	if err != nil {
		return intents.Ambiguous("invalid retained tier change payment: " + err.Error())
	}
	if receiptFound && progress.refused() != nil {
		return intents.Ambiguous("retained paid proration contradicts mutable refusal metadata")
	}
	if step := progress.refused(); step != nil {
		return h.terminal(ctx, in, intents.TerminalWithEvidence(step.Refusal, evidence()))
	}
	client, err := h.Checkout.resolveNMIClient(db.WithPSPID(ctx, *in.PspID), nmiIntentClientName(p.PSP, in.Rail))
	if err != nil {
		return intents.Parked(err.Error())
	}
	if client == nil {
		return intents.Parked("tier change provider account is unavailable")
	}
	owner, account := client.AccountIdentity()
	if owner != in.MerchantID || account != *in.PspID {
		return intents.Parked("tier change reader is armed for another provider account")
	}
	charged := p.ProrationAmount > 0
	if charged && progress.Proration == nil {
		if !send {
			return intents.Retryable("proration has not been submitted; execute under provider write gates")
		}
		if client.ReadOnly {
			return intents.Parked("NMI writes are disabled")
		}
		sub, err := h.Checkout.SubscriptionService.GetByID(ctx, p.OldSubscriptionID)
		if err != nil {
			return intents.Parked(err.Error())
		}
		if sub.Status != models.StatusActive || sub.PriceID != p.OldPriceID || sub.PspID != *in.PspID || sub.RailSubscriptionID != p.OldProviderSubscriptionID || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(p.PeriodEnd) {
			return h.terminal(ctx, in, intents.Terminal("subscription changed before the tier change was charged"))
		}
		progress.Proration = &nmiUpgradeStep{SubmittedAt: h.Checkout.now().UTC()}
		claimed, err := store.RecordProgressIfAbsent(ctx, in.ID, "proration", progress.Proration)
		if err != nil {
			return intents.Ambiguous("persist proration submission: " + err.Error())
		}
		if !claimed {
			return intents.Ambiguous("proration submission already owned; reconcile receipt")
		}
		credential := charge.InitialOneTime()
		if p.Instrument.StoredCredentialUnscheduledRef != "" {
			credential = charge.OneTimeReuse(p.Instrument.StoredCredentialUnscheduledRef)
		}
		sale, callErr := client.RunSale(ctx, nmi.SaleParams{CustomerVaultID: p.Instrument.RailCustomerRef, BillingID: p.Instrument.RailMethodRef, Amount: moneyutil.Cents(proration), Currency: p.Currency, OrderID: in.ID.String(), OrderDescription: "Upgrade: " + p.ProductName, StoredCredential: nmidirect.StoredCredentialFor(credential)})
		if callErr != nil {
			if errors.Is(callErr, nmi.ErrDuplicateTransaction) {
				// NMI's duplicate check refused this unique order unprocessed.
				progress.Proration.refuseUnexecuted(openrails.CodePaymentDuplicateRefused, duplicateProrationRefusal)
				if err = save("proration", progress.Proration); err != nil {
					return intents.AmbiguousWithEvidence("persist proration refusal: "+err.Error(), evidence())
				}
				return h.terminal(ctx, in, intents.TerminalWithEvidence(progress.Proration.Refusal, evidence()))
			}
			if nmi.RequiresVerification(callErr) {
				return intents.Ambiguous("proration submission has no exact receipt: " + callErr.Error())
			}
			progress.Proration.refuse(callErr)
			if err = save("proration", progress.Proration); err != nil {
				return intents.AmbiguousWithEvidence("persist proration refusal: "+err.Error(), evidence())
			}
			return h.terminal(ctx, in, intents.TerminalWithEvidence(callErr.Error(), evidence()))
		}
		if sale == nil || sale.TransactionID == "" {
			return intents.Ambiguous("proration response omitted transaction identity")
		}
		progress.Proration.Sale = sale
		if err = save("proration", progress.Proration); err != nil {
			return intents.AmbiguousWithEvidence("persist proration receipt: "+err.Error(), evidence())
		}
	}
	if charged && !receiptFound {
		candidate := ""
		if progress.Proration.Sale != nil {
			candidate = progress.Proration.Sale.TransactionID
		}
		receipt, receiptFound, err = intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, candidate)
		if err != nil {
			return intents.Ambiguous("tier change proration receipt does not qualify: " + err.Error())
		}
		if !receiptFound {
			return h.absentProration(ctx, in, p, client, progress.Proration, save, evidence)
		}
		receipt, err = store.RetainCollectedReceipt(ctx, in, receipt)
		if err != nil {
			return intents.Ambiguous("retain tier change proration receipt: " + err.Error())
		}
		progress.Proration.Sale = &nmi.SaleResponse{TransactionID: receipt.TransactionID()}
		if err = save("proration", progress.Proration); err != nil {
			return intents.Ambiguous("persist qualified proration reference: " + err.Error())
		}
	}
	if progress.Update == nil || !progress.Update.Done {
		if progress.Update == nil {
			progress.Update = &nmiScheduleUpdate{}
		}
		if client.ReadOnly {
			return intents.Parked("NMI writes are disabled")
		}
		if err := h.pushScheduleAmount(ctx, client, p); err != nil {
			progress.Update.Attempts++
			progress.Update.LastError = err.Error()
			if serr := save("schedule_update", progress.Update); serr != nil {
				return intents.AmbiguousWithEvidence("persist schedule update attempt: "+serr.Error(), evidence())
			}
			if progress.Update.Attempts >= nmiUpdateStuckAttempts {
				h.raiseUpdateStuck(ctx, in, p, err)
			}
			return intents.Retryable("NMI schedule amount update pending: " + err.Error())
		}
		progress.Update.Done, progress.Update.LastError = true, ""
		if err := save("schedule_update", progress.Update); err != nil {
			return intents.AmbiguousWithEvidence("persist schedule update: "+err.Error(), evidence())
		}
	}
	out := evidence()
	out["subscription_id"] = p.OldSubscriptionID.String()
	if p.Downgrade() {
		out["message"] = "Downgrade to " + p.ProductName + " scheduled for the end of the current period"
	} else {
		out["message"] = "Upgraded to " + p.ProductName
	}
	if progress.Proration != nil && progress.Proration.Sale != nil {
		out["transaction_id"] = progress.Proration.Sale.TransactionID
	}
	outcome := intents.Succeeded(out)
	if err = h.finalize(ctx, in, p, receipt, outcome); err != nil {
		return intents.Ambiguous("tier change receipts retained; local commit pending: " + err.Error())
	}
	return outcome
}

// pushScheduleAmount moves the existing NMI schedule to the accepted recurring
// amount (Direct Post recurring=update_subscription; NMI has no working v5
// update route) and verifies it by readback; the next billing date must not
// move. A custom schedule takes plan_amount (plan_payments preserved, no
// frequency or date). NMI ignores plan_amount on a named-plan schedule, so a
// named plan switches to the target price's linked plan instead. The mode is
// decided from a fresh read of the schedule on every attempt, so an operation
// admitted before a plan was linked converges once the link exists. A
// schedule already at the target is left untouched.
func (h *NMIUpgradeIntentHandler) pushScheduleAmount(ctx context.Context, client *nmi.NMIClient, p subscriptions.NMIUpgradePayload) error {
	cents, err := moneyutil.NativeToRailMinorExact(p.Currency, p.RecurringAmount)
	if err != nil {
		return err
	}
	remote, found, err := client.GetSubscription(ctx, p.OldProviderSubscriptionID)
	if err != nil {
		return fmt.Errorf("read schedule: %w", err)
	}
	if !found || remote.ID != p.OldProviderSubscriptionID {
		return fmt.Errorf("schedule %s is not live at NMI", p.OldProviderSubscriptionID)
	}
	if v := remote.CustomerVaultID; v != "" && v != p.Instrument.RailCustomerRef {
		return fmt.Errorf("schedule %s bills another vault", p.OldProviderSubscriptionID)
	}
	planID := ""
	if remote.NamedPlan() {
		if planID, err = h.targetPlan(ctx, client, p); err != nil {
			return err
		}
	}
	atTarget := func(s nmi.V5Subscription) bool {
		got, err := nmi.SubscriptionAmountMinor(s, p.Currency)
		return err == nil && got == cents && (planID == "" || (s.Plan != nil && strings.TrimSpace(s.Plan.ID) == planID))
	}
	if atTarget(remote) {
		return nil
	}
	if planID != "" {
		if err := client.UpdateRecurringSubscriptionPlan(ctx, p.OldProviderSubscriptionID, planID); err != nil {
			return fmt.Errorf("update schedule plan: %w", err)
		}
	} else {
		payments := 0
		if remote.Plan != nil && remote.Plan.PlanPayments != "" {
			if payments, err = strconv.Atoi(remote.Plan.PlanPayments); err != nil || payments < 0 {
				return fmt.Errorf("schedule %s has unparseable plan_payments", p.OldProviderSubscriptionID)
			}
		}
		wire, err := nmi.WireAmount(cents, p.Currency)
		if err != nil {
			return err
		}
		if _, err := client.UpdateRecurringSubscription(ctx, p.OldProviderSubscriptionID, wire, payments); err != nil {
			return fmt.Errorf("update schedule: %w", err)
		}
	}
	after, found, err := client.GetSubscription(ctx, p.OldProviderSubscriptionID)
	if err != nil || !found {
		return fmt.Errorf("verify schedule: %v", err)
	}
	if !atTarget(after) {
		plan := ""
		if after.Plan != nil {
			plan = after.Plan.ID
		}
		return fmt.Errorf("schedule %s reads amount %s on plan %q after the update", p.OldProviderSubscriptionID, after.Amount, plan)
	}
	if after.NextBillingDate != remote.NextBillingDate {
		return fmt.Errorf("schedule %s next billing date moved from %s to %s", p.OldProviderSubscriptionID, remote.NextBillingDate, after.NextBillingDate)
	}
	return nil
}

// targetPlan is the named plan a named-plan schedule switches to: the plan
// frozen at admission, else (an operation admitted before the price was
// linked) the target price's current link, verified at NMI.
func (h *NMIUpgradeIntentHandler) targetPlan(ctx context.Context, client *nmi.NMIClient, p subscriptions.NMIUpgradePayload) (string, error) {
	price, err := h.Checkout.PriceService.GetByID(ctx, p.PriceID)
	if err != nil {
		return "", fmt.Errorf("load target price: %w", err)
	}
	link := map[string]string{"plan_id": p.TargetPlanID}
	if p.TargetPlanID == "" {
		rt, err := h.Checkout.resolveRailTargetForPSP(ctx, "nmi", p.Instrument.PSPID)
		if err != nil {
			return "", err
		}
		link = checkoutPSPLinkForTarget(price, rt)
	}
	planID, err := linkedTargetPlan(ctx, client, price, link)
	if err != nil {
		return "", fmt.Errorf("schedule %s is on a named NMI plan and the target price has no linked NMI plan of the same amount and cycle; link one in the catalog", p.OldProviderSubscriptionID)
	}
	return planID, nil
}

// absentProration decides a submitted proration with no receipt from the
// provider's record under the operation's order: nothing after the settle
// delay means nothing was charged; a lone refused sale is a decline. A read
// that cannot settle it raises the operator finding.
func (h *NMIUpgradeIntentHandler) absentProration(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIUpgradePayload, client *nmi.NMIClient, step *nmiUpgradeStep, save func(string, any) error, evidence func() map[string]any) intents.Outcome {
	if h.Checkout.now().Before(step.SubmittedAt.Add(intents.LostSubmissionSettle)) {
		return intents.Ambiguous("tier change proration receipt is not yet visible")
	}
	attempts, err := client.ReadOrderAttempts(ctx, in.ID.String())
	switch {
	case err != nil:
		h.raiseProrationUnresolved(ctx, in, p, "the provider's transaction search failed: "+err.Error())
		return intents.Ambiguous("tier change proration cannot be read: " + err.Error())
	case attempts.Transactions == 0:
		step.refuseUnexecuted(openrails.CodeTierChangeRefused, absentProrationRefusal)
	case attempts.Declined:
		step.refuse(&nmi.CustomerVaultError{Message: "sale declined", ResponseCode: attempts.DeclineCode, LocalizationID: nmi.LocalizationIDForResponseCode(attempts.DeclineCode)})
	default:
		h.raiseProrationUnresolved(ctx, in, p, fmt.Sprintf("the provider holds %d transaction(s) under this order but none qualifies as the accepted charge", attempts.Transactions))
		return intents.Ambiguous("tier change proration receipt does not qualify")
	}
	if err := save("proration", step); err != nil {
		return intents.AmbiguousWithEvidence("persist proration outcome: "+err.Error(), evidence())
	}
	h.closeFinding(ctx, in.MerchantID, ProrationUnresolvedFinding, in.ID.String())
	return h.terminal(ctx, in, intents.TerminalWithEvidence(step.Refusal, evidence()))
}

func (h *NMIUpgradeIntentHandler) closeFinding(ctx context.Context, merchantID uuid.UUID, findingType, subject string) {
	wctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	q := h.Checkout.SubscriptionService.Database().Gen(wctx)
	if row, err := q.GetReconciliationFindingByIdentity(wctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: merchantID, FindingType: findingType, SubjectKey: subject}); err == nil {
		_, _ = q.MarkReconciliationFindingVanished(wctx, gen.MarkReconciliationFindingVanishedParams{MerchantID: merchantID, ID: row.ID})
	}
}

func (h *NMIUpgradeIntentHandler) raiseProrationUnresolved(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIUpgradePayload, reason string) {
	raw, _ := json.Marshal(map[string]any{"operation_id": in.ID.String(), "subscription_id": openrails.SubscriptionID(p.OldSubscriptionID).String(), "order_id": in.ID.String(), "reason": reason})
	action := fmt.Sprintf("A tier change charge (order %s) cannot be settled from NMI: %s. Nothing further is charged while this stands. Confirm at NMI, then run `openrails intents resolve --intent %s --step proration --receipt <transaction id>` if it was charged, or `--not-executed` if NMI holds no transaction for the order.", in.ID, reason, in.ID)
	wctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	_, _ = h.Checkout.SubscriptionService.Database().Gen(wctx).UpsertReconciliationFinding(wctx, gen.UpsertReconciliationFindingParams{MerchantID: in.MerchantID, FindingType: ProrationUnresolvedFinding,
		SubjectKey: in.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: raw})
}

func (h *NMIUpgradeIntentHandler) raiseUpdateStuck(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIUpgradePayload, cause error) {
	raw, _ := json.Marshal(map[string]any{"operation_id": in.ID.String(), "subscription_id": openrails.SubscriptionID(p.OldSubscriptionID).String(),
		"rail_subscription_id": p.OldProviderSubscriptionID, "action": p.Action, "error": cause.Error()})
	action := "A tier change could not move the NMI schedule to its new amount; NMI still bills the previous amount. If the schedule is on a named NMI plan, link the target price to an NMI plan of the same amount and cycle; the operation keeps retrying and completes once NMI accepts the change."
	wctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	_, _ = h.Checkout.SubscriptionService.Database().Gen(wctx).UpsertReconciliationFinding(wctx, gen.UpsertReconciliationFindingParams{MerchantID: in.MerchantID, FindingType: ProviderUpdateStuckFinding,
		SubjectKey: p.OldSubscriptionID.String(), Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: raw})
}

// Resolve accepts exact provider evidence for a submitted proration that has
// no receipt; the verifier then converges through the same path.
func (h *NMIUpgradeIntentHandler) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	if h.Checkout == nil || h.Checkout.Lifecycle == nil {
		return intents.Outcome{}, errors.New("tier change lifecycle unavailable")
	}
	p, err := subscriptions.DecodeNMIUpgradePayload(in)
	if err != nil {
		return intents.Outcome{}, err
	}
	var progress nmiUpgradeProgress
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &progress); err != nil {
			return intents.Outcome{}, err
		}
	}
	if resolution.Step != "proration" {
		return intents.Outcome{}, fmt.Errorf("%w: tier change step must be proration", intents.ErrResolutionInvalid)
	}
	step := progress.Proration
	if step == nil {
		return intents.Outcome{}, intents.RejectResolution("proration step was never submitted")
	}
	if step.Refusal != "" {
		return intents.Outcome{}, intents.RejectResolution("proration step already has an outcome")
	}
	client, err := h.Checkout.resolveNMIClient(db.WithPSPID(ctx, *in.PspID), nmiIntentClientName(p.PSP, in.Rail))
	if err != nil {
		return intents.Outcome{}, fmt.Errorf("resolve nmi client: %w", err)
	}
	if client == nil {
		return intents.Outcome{}, intents.RejectResolution("provider account reader is unavailable")
	}
	owner, account := client.AccountIdentity()
	if owner != in.MerchantID || account != *in.PspID {
		return intents.Outcome{}, intents.RejectResolution("provider reader names another account")
	}
	store := intents.NewStore(h.Checkout.SubscriptionService.Database())
	if resolution.NotExecuted {
		// Nonexecution is proven by NMI holding no transaction under the
		// operation's unique order reference.
		attempts, err := client.ReadOrderAttempts(ctx, in.ID.String())
		if err != nil {
			return intents.Outcome{}, intents.RejectResolution("NMI transaction search is unavailable: %v", err)
		}
		if attempts.Transactions != 0 {
			return intents.Outcome{}, intents.RejectResolution("NMI holds %d transaction(s) under this order; resolve with its receipt", attempts.Transactions)
		}
		step.refuseUnexecuted(openrails.CodeTierChangeRefused, absentProrationRefusal)
		step.Resolution = resolution.Record(h.Checkout.now())
		if err := store.RecordProgress(ctx, in.ID, map[string]any{"proration": step}); err != nil {
			return intents.Outcome{}, fmt.Errorf("persist resolved proration step: %w", err)
		}
		return h.advance(ctx, in, false), nil
	}
	receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, resolution.ProviderReference)
	if err != nil || !found {
		return intents.Outcome{}, intents.RejectResolution("exact tier change proration receipt is unavailable or contradicts accepted terms")
	}
	if _, err = store.RetainCollectedReceipt(ctx, in, receipt); err != nil {
		return intents.Outcome{}, err
	}
	step.Sale = &nmi.SaleResponse{TransactionID: receipt.TransactionID()}
	step.Resolution = resolution.Record(h.Checkout.now())
	if err := store.RecordProgress(ctx, in.ID, map[string]any{"proration": step}); err != nil {
		return intents.Outcome{}, fmt.Errorf("persist resolved proration step: %w", err)
	}
	return h.advance(ctx, in, false), nil
}

// finalize commits the accepted change on the same subscription, keeping its
// period end: an upgrade switches price, product and access now and records
// the proration payment; a downgrade schedules the price for the renewal NMI
// already bills at the new amount.
func (h *NMIUpgradeIntentHandler) finalize(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIUpgradePayload, receipt intents.CollectedReceipt, outcome intents.Outcome) error {
	database := h.Checkout.SubscriptionService.Database()
	customer, err := customerIDFromUser(p.UserID)
	if err != nil {
		return err
	}
	var payment *models.Payment
	if p.ProrationAmount > 0 {
		if err := receipt.Validate(in); err != nil {
			return err
		}
		payment = &models.Payment{ID: p.NewPaymentID, CustomerID: customer, PriceID: p.PriceID, SubscriptionID: &p.OldSubscriptionID, Rail: models.Rail(in.Rail), PspID: in.PspID, TransactionID: receipt.TransactionID(), Amount: p.ProrationAmount, ListAmount: p.RecurringAmount, Currency: p.Currency, Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: p.PeriodStart, EntitlementsSpecSnapshot: p.Entitlements, Metadata: map[string]any{"upgrade_intent_id": in.ID.String(), subscriptions.PaidPeriodKey: p.PeriodStart.UTC().Format(time.RFC3339)}}
	}
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := database.NewWithPgxTx(tx)
		if _, err := subscriptions.NewSubscriptionRepo(txDB).GetByIDForUpdate(ctx, p.OldSubscriptionID); err != nil {
			return err
		}
		completion, err := prepareTierCompletion(ctx, txDB, in, outcome, h.Checkout.now())
		if err != nil {
			return err
		}
		if completion.committed {
			return nil
		}
		change := subscriptions.InPlaceTierChange{SubscriptionID: p.OldSubscriptionID, FromPriceID: p.OldPriceID, PriceID: p.PriceID, ProductID: p.ProductID, RailSubscriptionID: p.OldProviderSubscriptionID, PeriodEnd: p.PeriodEnd, At: p.PeriodStart, Entitlements: p.Entitlements, Payment: payment, Downgrade: p.Downgrade()}
		if err := h.Checkout.Lifecycle.ChangeTierInPlaceTx(ctx, txDB, change); err != nil {
			return err
		}
		if payment != nil {
			if _, err := txDB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, Agreement: "unscheduled", Ref: payment.TransactionID}); err != nil {
				return err
			}
		}
		for finding, subject := range map[string]string{ProviderUpdateStuckFinding: p.OldSubscriptionID.String(), ProrationUnresolvedFinding: in.ID.String()} {
			if row, err := txDB.Gen(ctx).GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: in.MerchantID, FindingType: finding, SubjectKey: subject}); err == nil {
				if _, err := txDB.Gen(ctx).MarkReconciliationFindingVanished(ctx, gen.MarkReconciliationFindingVanishedParams{MerchantID: in.MerchantID, ID: row.ID}); err != nil {
					return err
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return completion.commit(ctx)
	})
	return err
}

// nmiUpgradeTierChangeResponse renders an NMI upgrade (tierChangeResponse).
// While unresolved it names the predecessor the operation owns; once
// committed, the successor.
func nmiUpgradeTierChangeResponse(in gen.OpenrailsRailIntent) (*TierChangeResponse, error) {
	var p subscriptions.NMIUpgradePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return nil, err
	}
	subID := openrails.SubscriptionID(p.OldSubscriptionID)
	end := p.PeriodEnd
	action := "upgrade"
	if p.Downgrade() {
		action = "downgrade"
	}
	resp := &TierChangeResponse{
		Object: "tier_change", Mode: "tier_change", Action: action, Effective: effectiveOf(action), PriceID: (openrails.PriceID(p.PriceID)).String(),
		Payment: CheckoutSessionPaymentResponse{Rail: in.Rail}, SubscriptionID: &subID,
		Currency: p.Currency, AmountDueNow: p.ProrationAmount, NextChargeAmount: p.RecurringAmount, NextChargeDate: &end,
		OperationID: in.ID.String(),
	}
	if p.Downgrade() {
		resp.DelayedStart = &end
	}
	switch in.Status {
	case intents.StatusSucceeded:
		resp.Status = "succeeded"
		resp.Payment.TransactionID = intents.EvidenceString(in, "transaction_id")
		resp.Message = intents.EvidenceString(in, "message")
		return resp, nil
	case intents.StatusFailedTerminal:
		var progress nmiUpgradeProgress
		_ = json.Unmarshal(in.ResultEvidence, &progress)
		if step := progress.refused(); step != nil {
			return nil, tierChangeRefused(in, step.RefusalStatus, step.RefusalCode)
		}
		return nil, tierChangeRefused(in, 0, "")
	default:
		return tierChangeProcessing(resp)
	}
}

// upgradeReceiptResolver adapts the account already armed from the accepted
// operation. The shared reader verifies its merchant/PSP binding before HTTP.
type upgradeReceiptResolver struct{ client *nmi.NMIClient }

func (r upgradeReceiptResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return r.client, r.client != nil, nil
}

func (h *NMIUpgradeIntentHandler) terminal(ctx context.Context, in gen.OpenrailsRailIntent, outcome intents.Outcome) intents.Outcome {
	return commitTierRefusal(ctx, h.Checkout.SubscriptionService.Database(), in, outcome, h.Checkout.now())
}
