package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	SubmittedAt time.Time                    `json:"submitted_at"`
	Enrollment  *nmi.AddSubscriptionResponse `json:"enrollment,omitempty"`
	Sale        *nmi.SaleResponse            `json:"sale,omitempty"`
	Refusal     string                       `json:"refusal,omitempty"`
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

type nmiUpgradeProgress struct {
	Successor *nmiUpgradeStep `json:"successor,omitempty"`
	Proration *nmiUpgradeStep `json:"proration,omitempty"`
}

func (p nmiUpgradeProgress) refused() *nmiUpgradeStep {
	for _, step := range []*nmiUpgradeStep{p.Successor, p.Proration} {
		if step != nil && step.Refusal != "" {
			return step
		}
	}
	return nil
}

// NMIUpgradeIntentHandler resumes each provider step independently and commits
// the local effects only after both required receipts are durable.
type NMIUpgradeIntentHandler struct{ Checkout *CheckoutService }

func NewNMIUpgradeIntentHandler(s *CheckoutService) *NMIUpgradeIntentHandler {
	return &NMIUpgradeIntentHandler{Checkout: s}
}
func (*NMIUpgradeIntentHandler) Type() string { return TypeNMIUpgrade }
func (*NMIUpgradeIntentHandler) Backoff(attempts int32) time.Duration {
	return intents.DefaultBackoff.Delay(attempts)
}
func (*NMIUpgradeIntentHandler) PrunePolicy() (bool, bool) { return true, true }
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
		return intents.Parked("upgrade lifecycle unavailable")
	}
	p, err := subscriptions.DecodeNMIUpgradePayload(in)
	if err != nil {
		return intents.Terminal("invalid upgrade payload: " + err.Error())
	}
	recurring, err := moneyutil.NativeToRailMinorExact(p.Currency, p.RecurringAmount)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	proration, err := moneyutil.NativeToRailMinorExact(p.Currency, p.ProrationAmount)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	database := h.Checkout.SubscriptionService.Database()
	store := intents.NewStore(database)
	// Always reload progress: a preceding submission may have committed its
	// receipt immediately before this lease was reclaimed.
	current, err := store.Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous("read upgrade progress: " + err.Error())
	}
	var progress nmiUpgradeProgress
	if len(current.ResultEvidence) > 0 {
		if err = json.Unmarshal(current.ResultEvidence, &progress); err != nil {
			return intents.Ambiguous("read upgrade receipts: " + err.Error())
		}
	}
	evidence := func() map[string]any {
		return map[string]any{"successor": progress.Successor, "proration": progress.Proration}
	}
	// A step's receipt or refusal is provider truth already in hand: persist it
	// detached from the caller's cancellation, and carry it on any ambiguous
	// outcome so the runner's unknown mark retains it even if this write fails.
	save := func(key string, step *nmiUpgradeStep) error {
		wctx, cancel := intents.LedgerWriteContext(ctx)
		defer cancel()
		return store.RecordProgress(wctx, in.ID, map[string]any{key: step})
	}
	receipt, receiptFound, err := intents.LoadCollectedReceipt(current)
	if err != nil {
		return intents.Ambiguous("invalid retained upgrade payment: " + err.Error())
	}
	enrollment, enrollmentFound, err := intents.LoadNMIEnrollmentReceipt(current)
	if err != nil {
		return intents.Ambiguous("invalid retained successor: " + err.Error())
	}
	if enrollmentFound {
		if progress.Successor == nil || progress.Successor.Refusal != "" {
			return intents.Ambiguous("retained enrollment contradicts its submission progress")
		}
		progress.Successor.Enrollment = &nmi.AddSubscriptionResponse{SubscriptionID: enrollment.SubscriptionID()}
	}
	if (receiptFound || progress.Proration != nil) && !enrollmentFound {
		return intents.Ambiguous("proration has no qualified predecessor enrollment")
	}
	if receiptFound && progress.refused() != nil {
		return intents.Ambiguous("retained paid proration contradicts mutable refusal metadata")
	}
	if receiptFound && (progress.Successor == nil || progress.Successor.Enrollment == nil || progress.Successor.Enrollment.SubscriptionID == "" || progress.Proration == nil) {
		return intents.Ambiguous("retained paid proration has incomplete predecessor step evidence")
	}
	if progress.Successor != nil && progress.Successor.Refusal != "" {
		return intents.TerminalWithEvidence(progress.Successor.Refusal, evidence())
	}
	if progress.Proration != nil && progress.Proration.Refusal != "" {
		return h.refusedProration(ctx, in, p, progress, evidence(), enrollment)
	}
	var client *nmi.NMIClient
	needsClient := !enrollmentFound || (p.ProrationAmount > 0 && !receiptFound)
	if needsClient {
		client, err = h.Checkout.resolveNMIClient(db.WithPSPID(ctx, *in.PspID), nmiIntentClientName(p.PSP, in.Rail))
		if err != nil {
			return intents.Parked(err.Error())
		}
		if client == nil {
			return intents.Parked("upgrade provider account is unavailable")
		}
		owner, account := client.AccountIdentity()
		if owner != in.MerchantID || account != *in.PspID {
			return intents.Parked("upgrade reader is armed for another provider account")
		}
	}
	if progress.Successor == nil {
		if !send {
			return intents.Retryable("successor step was not submitted; execute under provider write gates")
		}
		if client.ReadOnly {
			return intents.Parked("NMI writes are disabled")
		}
		old, err := h.Checkout.SubscriptionService.GetByID(ctx, p.OldSubscriptionID)
		if err != nil {
			return intents.Parked(err.Error())
		}
		if old.Status != models.StatusActive || old.PriceID != p.OldPriceID || old.PspID != *in.PspID {
			return intents.Terminal("upgrade predecessor changed before submission")
		}
		progress.Successor = &nmiUpgradeStep{SubmittedAt: h.Checkout.now().UTC()}
		claimed, err := store.RecordProgressIfAbsent(ctx, in.ID, "successor", progress.Successor)
		if err != nil {
			return intents.Parked("persist successor submission: " + err.Error())
		}
		if !claimed {
			return intents.Ambiguous("successor submission already owned; reconcile receipt")
		}
		credential := charge.InitialRecurring()
		if p.Instrument.StoredCredentialRecurringRef != "" {
			credential = charge.RecurringReuse(p.Instrument.StoredCredentialRecurringRef)
		}
		order := intents.NMIEnrollmentOrder(in)
		receipt, callErr := client.AddRecurringSubscription(ctx, nmi.RecurringPaymentData{CardUserData: p.Card, PlanID: p.PlanID, CustomerVaultID: p.Instrument.RailCustomerRef, BillingID: p.Instrument.RailMethodRef, Amount: moneyutil.Cents(recurring), Currency: p.Currency, Email: p.Email, CustomerID: p.UserID, OrderID: order, PONumber: order, StartDate: p.StartDate, StoredCredential: nmidirect.StoredCredentialFor(credential)})
		if callErr != nil {
			if nmi.RequiresVerification(callErr) {
				return intents.Ambiguous("successor submission has no exact receipt: " + callErr.Error())
			}
			progress.Successor.refuse(callErr)
			if err = save("successor", progress.Successor); err != nil {
				return intents.AmbiguousWithEvidence("persist successor refusal: "+err.Error(), evidence())
			}
			return intents.TerminalWithEvidence(callErr.Error(), evidence())
		}
		if receipt == nil || receipt.SubscriptionID == "" {
			return intents.Ambiguous("successor response omitted subscription identity")
		}
		progress.Successor.Enrollment = receipt
		if err = save("successor", progress.Successor); err != nil {
			return intents.AmbiguousWithEvidence("persist successor receipt: "+err.Error(), evidence())
		}
	}
	if progress.Successor.Enrollment == nil {
		// The NMI roster exposes vault/plan, not the immutable upgrade order.
		// A unique matching roster row is not proof that THIS request created it.
		return intents.Ambiguous("successor submission needs an exact account-scoped provider receipt; roster similarity cannot authorize adoption or resend")
	}
	if !enrollmentFound {
		enrollment, enrollmentFound, err = intents.ReadNMIEnrollmentReceipt(ctx, in, upgradeReceiptResolver{client}, progress.Successor.Enrollment.SubscriptionID)
		if err != nil || !enrollmentFound {
			return intents.Ambiguous("successor schedule does not qualify for the accepted upgrade")
		}
		enrollment, err = store.RetainNMIEnrollmentReceipt(ctx, in, enrollment)
		if err != nil {
			return intents.Ambiguous("retain successor enrollment: " + err.Error())
		}
	}
	progress.Successor.Enrollment = &nmi.AddSubscriptionResponse{SubscriptionID: enrollment.SubscriptionID()}
	if err = save("successor", progress.Successor); err != nil {
		return intents.Ambiguous("persist qualified successor reference: " + err.Error())
	}
	if p.ProrationAmount > 0 {
		order := in.ID.String()
		if progress.Proration == nil {
			if !send {
				return intents.Retryable("successor receipt recovered; proration has not been submitted")
			}
			if client.ReadOnly {
				return intents.Parked("NMI writes are disabled")
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
			receipt, callErr := client.RunSale(ctx, nmi.SaleParams{CustomerVaultID: p.Instrument.RailCustomerRef, BillingID: p.Instrument.RailMethodRef, Amount: moneyutil.Cents(proration), Currency: p.Currency, OrderID: order, OrderDescription: "Upgrade: " + p.ProductName, StoredCredential: nmidirect.StoredCredentialFor(credential)})
			if callErr != nil {
				if nmi.RequiresVerification(callErr) {
					return intents.Ambiguous("proration submission has no exact receipt: " + callErr.Error())
				}
				progress.Proration.refuse(callErr)
				if err = save("proration", progress.Proration); err != nil {
					return intents.AmbiguousWithEvidence("persist proration refusal: "+err.Error(), evidence())
				}
				return h.refusedProration(ctx, in, p, progress, evidence(), enrollment)
			}
			if receipt == nil || receipt.TransactionID == "" {
				return intents.Ambiguous("proration response omitted transaction identity")
			}
			progress.Proration.Sale = receipt
			if err = save("proration", progress.Proration); err != nil {
				return intents.AmbiguousWithEvidence("persist proration receipt: "+err.Error(), evidence())
			}
		}
		if !receiptFound {
			candidate := ""
			if progress.Proration.Sale != nil {
				candidate = progress.Proration.Sale.TransactionID
			}
			receipt, receiptFound, err = intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, candidate)
			if err != nil {
				return intents.Ambiguous("upgrade proration receipt does not qualify: " + err.Error())
			}
			if !receiptFound {
				return intents.Ambiguous("upgrade proration receipt is not yet visible")
			}
			receipt, err = store.RetainCollectedReceipt(ctx, in, receipt)
			if err != nil {
				return intents.Ambiguous("retain upgrade proration receipt: " + err.Error())
			}
		}
		// Classic responses and search hits are candidates, never authority.
		progress.Proration.Sale = &nmi.SaleResponse{TransactionID: receipt.TransactionID()}
		if err = save("proration", progress.Proration); err != nil {
			return intents.Ambiguous("persist qualified upgrade reference: " + err.Error())
		}

	}
	if err = h.finalize(ctx, in, p, progress, receipt, enrollment); err != nil {
		return intents.Ambiguous("upgrade receipts retained; local commit pending: " + err.Error())
	}
	out := evidence()
	out["subscription_id"] = p.NewSubscriptionID.String()
	out["message"] = "Upgraded to " + p.ProductName
	if progress.Proration != nil && progress.Proration.Sale != nil {
		out["transaction_id"] = progress.Proration.Sale.TransactionID
	}
	return intents.Succeeded(out)
}

// Resolve accepts exact provider evidence for one submitted step that has no
// receipt. A successor reference must be a live subscription on the frozen
// vault and plan that no other local subscription owns; a proration reference
// must be an approved sale of the frozen amount on the frozen vault.
// Non-execution applies the step's definitive-refusal path. The recorded step
// then converges through the same verifier path as a provider receipt, so an
// unsent proration is still submitted only by the executor.
func (h *NMIUpgradeIntentHandler) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	if h.Checkout == nil || h.Checkout.Lifecycle == nil {
		return intents.Outcome{}, errors.New("upgrade lifecycle unavailable")
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
	var step *nmiUpgradeStep
	switch resolution.Step {
	case "successor":
		step = progress.Successor
	case "proration":
		step = progress.Proration
	default:
		return intents.Outcome{}, fmt.Errorf("%w: upgrade step must be successor or proration", intents.ErrResolutionInvalid)
	}
	if step == nil {
		return intents.Outcome{}, intents.RejectResolution("%s step was never submitted", resolution.Step)
	}
	if step.Refusal != "" {
		return intents.Outcome{}, intents.RejectResolution("%s step already has an outcome", resolution.Step)
	}
	if resolution.NotExecuted {
		return intents.Outcome{}, intents.RejectResolution("submitted NMI %s has no authoritative nonexecution proof", resolution.Step)
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
	ref := resolution.ProviderReference
	switch {
	case resolution.Step == "successor":
		enrollment, found, err := intents.ReadNMIEnrollmentReceipt(ctx, in, upgradeReceiptResolver{client}, ref)
		if err != nil || !found {
			return intents.Outcome{}, intents.RejectResolution("exact successor schedule is unavailable or contradicts accepted terms")
		}

		local, err := h.Checkout.SubscriptionService.GetByPSPSubscriptionID(db.WithPSPID(ctx, *in.PspID), in.Rail, ref)
		switch {
		case err == nil && local.ID != p.NewSubscriptionID:
			return intents.Outcome{}, intents.RejectResolution("subscription %s is already registered to local subscription %s", ref, local.ID)
		case err != nil && !db.IsNotFound(err):
			return intents.Outcome{}, err
		}
		if _, err := intents.NewStore(h.Checkout.SubscriptionService.Database()).RetainNMIEnrollmentReceipt(ctx, in, enrollment); err != nil {
			return intents.Outcome{}, err
		}
		step.Enrollment = &nmi.AddSubscriptionResponse{SubscriptionID: enrollment.SubscriptionID()}
	default:
		receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, ref)
		if err != nil || !found {
			return intents.Outcome{}, intents.RejectResolution("exact upgrade proration receipt is unavailable or contradicts accepted terms")
		}
		if _, err = intents.NewStore(h.Checkout.SubscriptionService.Database()).RetainCollectedReceipt(ctx, in, receipt); err != nil {
			return intents.Outcome{}, err
		}
		step.Sale = &nmi.SaleResponse{TransactionID: receipt.TransactionID()}

	}
	step.Resolution = resolution.Record(h.Checkout.now())
	if err := intents.NewStore(h.Checkout.SubscriptionService.Database()).RecordProgress(ctx, in.ID, map[string]any{resolution.Step: step}); err != nil {
		return intents.Outcome{}, fmt.Errorf("persist resolved %s step: %w", resolution.Step, err)
	}
	return h.advance(ctx, in, false), nil
}

// A definitive proration refusal leaves an unpaid successor schedule. Retain
// its exact receipt and queue the existing verify-then-delete operation; never
// resend the declined charge or abandon a live schedule through direct cleanup.
func (h *NMIUpgradeIntentHandler) refusedProration(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIUpgradePayload, progress nmiUpgradeProgress, evidence map[string]any, enrollment intents.NMIEnrollmentReceipt) intents.Outcome {
	if err := enrollment.Validate(in); err != nil {
		return intents.Ambiguous(err.Error())
	}
	database := h.Checkout.SubscriptionService.Database()
	customer, err := customerIDFromUser(p.UserID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := database.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(txDB)
		if _, err := repo.GetByIDForUpdate(ctx, p.OldSubscriptionID); err != nil {
			return err
		}
		if _, err := repo.GetByID(ctx, p.NewSubscriptionID); db.IsNotFound(err) {
			reason := models.CancelType("upgrade")
			sub := &models.Subscription{ID: p.NewSubscriptionID, CustomerID: customer, PspID: *in.PspID, ProductID: p.ProductID, PriceID: p.PriceID, Rail: models.Rail(in.Rail), RailSubscriptionID: progress.Successor.Enrollment.SubscriptionID, PaymentMethodID: &p.PaymentMethodID, Status: models.StatusCancelled, StartedAt: p.PeriodStart, CancelledAt: &p.PeriodStart, DeletionScheduledAt: &p.PeriodStart, CancelType: &reason}
			if err := repo.Create(ctx, sub); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		_, err := intents.NewStore(txDB).Enqueue(ctx, intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, SubscriptionID: &p.NewSubscriptionID, IntentType: intents.TypeNMIDeleteSubscription, Payload: intents.NMIDeletePayload{UserID: p.UserID, RailSubscriptionID: progress.Successor.Enrollment.SubscriptionID}, IdempotencyKey: intents.NMIDeleteIdempotencyKey(p.NewSubscriptionID, *in.PspID, progress.Successor.Enrollment.SubscriptionID), NextAttemptAt: p.PeriodStart, Origin: intents.OriginUser, OriginReason: "cancel unpaid upgrade successor after definitive proration refusal"})
		return err
	})
	if err != nil {
		return intents.AmbiguousWithEvidence("proration refused; durable successor cancellation pending: "+err.Error(), evidence)
	}
	return intents.TerminalWithEvidence("proration refused; unpaid successor queued for cancellation: "+progress.Proration.Refusal, evidence)
}

func (h *NMIUpgradeIntentHandler) finalize(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.NMIUpgradePayload, progress nmiUpgradeProgress, receipt intents.CollectedReceipt, enrollment intents.NMIEnrollmentReceipt) error {
	if err := enrollment.Validate(in); err != nil {
		return err
	}
	database := h.Checkout.SubscriptionService.Database()
	customer, err := customerIDFromUser(p.UserID)
	if err != nil {
		return err
	}
	next := &models.Subscription{ID: p.NewSubscriptionID, CustomerID: customer, PspID: *in.PspID, ProductID: p.ProductID, PriceID: p.PriceID, Rail: models.Rail(in.Rail), RailSubscriptionID: progress.Successor.Enrollment.SubscriptionID, PaymentMethodID: &p.PaymentMethodID, EntitlementsSpecSnapshot: p.Entitlements, Status: models.StatusActive, StartedAt: p.PeriodStart, CurrentPeriodStartsAt: &p.PeriodStart, CurrentPeriodEndsAt: &p.PeriodEnd}
	if p.Email != "" {
		next.UserEmail = &p.Email
	}
	var payment *models.Payment
	if p.ProrationAmount > 0 {
		if err := receipt.Validate(in); err != nil {
			return err
		}

		payment = &models.Payment{ID: p.NewPaymentID, CustomerID: customer, PriceID: p.PriceID, SubscriptionID: &next.ID, Rail: models.Rail(in.Rail), PspID: in.PspID, TransactionID: receipt.TransactionID(), Amount: p.ProrationAmount, ListAmount: p.RecurringAmount, Currency: p.Currency, Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: p.PeriodStart, EntitlementsSpecSnapshot: p.Entitlements, Metadata: map[string]any{"upgrade_intent_id": in.ID.String()}}
	}
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := database.NewWithPgxTx(tx)
		if err := h.Checkout.Lifecycle.CompleteUpgradeTx(ctx, txDB, models.Subscription{ID: p.OldSubscriptionID, PriceID: p.OldPriceID, RailSubscriptionID: p.OldProviderSubscriptionID}, next, payment); err != nil {
			return err
		}
		// A delayed enrollment may return its schedule ID as transactionid. It
		// is not a qualifying CIT and must not become a recurring agreement.
		if payment != nil {
			if _, err := txDB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, Agreement: "unscheduled", Ref: payment.TransactionID}); err != nil {
				return err
			}
		}

		_, err := intents.NewStore(txDB).Enqueue(ctx, intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, SubscriptionID: &p.OldSubscriptionID, IntentType: intents.TypeNMIDeleteSubscription, Payload: intents.NMIDeletePayload{UserID: p.UserID, RailSubscriptionID: p.OldProviderSubscriptionID}, IdempotencyKey: intents.NMIDeleteIdempotencyKey(p.OldSubscriptionID, *in.PspID, p.OldProviderSubscriptionID), NextAttemptAt: p.PeriodStart, Origin: intents.OriginUser, OriginReason: "cancel predecessor after durable tier upgrade"})
		return err
	})
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
	resp := &TierChangeResponse{
		Object: "tier_change", Mode: "tier_change", Action: "upgrade", PriceID: openrails.PriceID(p.PriceID),
		Payment: CheckoutSessionPaymentResponse{Rail: in.Rail}, SubscriptionID: &subID,
		Currency: p.Currency, AmountDueNow: p.ProrationAmount, NextChargeAmount: p.RecurringAmount, NextChargeDate: &end,
		OperationID: in.ID.String(),
	}
	switch in.Status {
	case intents.StatusSucceeded:
		successor := openrails.SubscriptionID(p.NewSubscriptionID)
		resp.Status, resp.SubscriptionID = "succeeded", &successor
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
