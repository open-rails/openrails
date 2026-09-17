package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
)

const TypeNMIUpgrade = "nmi_upgrade"

func NMIUpgradeIdempotencyKey(key string) string {
	return TypeNMIUpgrade + ":" + strings.TrimSpace(key)
}

// NMIUpgradePayload freezes the complete commercial decision before either
// provider submission. Replays never recalculate proration or the billing date.
type NMIUpgradePayload struct {
	RequestedPrice            string           `json:"requested_price"`
	PSP                       string           `json:"psp"`
	UserID                    string           `json:"user_id"`
	Email                     string           `json:"email"`
	OldSubscriptionID         uuid.UUID        `json:"old_subscription_id"`
	OldPriceID                uuid.UUID        `json:"old_price_id"`
	OldProviderSubscriptionID string           `json:"old_provider_subscription_id"`
	NewSubscriptionID         uuid.UUID        `json:"new_subscription_id"`
	NewPaymentID              uuid.UUID        `json:"new_payment_id"`
	PriceID                   uuid.UUID        `json:"price_id"`
	ProductID                 uuid.UUID        `json:"product_id"`
	ProductName               string           `json:"product_name"`
	PlanID                    string           `json:"plan_id"`
	VaultID                   string           `json:"vault_id"`
	BillingID                 string           `json:"billing_id"`
	PaymentMethodID           uuid.UUID        `json:"payment_method_id"`
	RecurringAmount           int64            `json:"recurring_amount"`
	ProrationAmount           int64            `json:"proration_amount"`
	Currency                  string           `json:"currency"`
	PeriodStart               time.Time        `json:"period_start"`
	PeriodEnd                 time.Time        `json:"period_end"`
	StartDate                 string           `json:"start_date"`
	RecurringAnchor           string           `json:"recurring_anchor"`
	UnscheduledAnchor         string           `json:"unscheduled_anchor"`
	Entitlements              map[string]*int  `json:"entitlements"`
	Card                      nmi.CardUserData `json:"card"`
}

type nmiUpgradeStep struct {
	SubmittedAt time.Time                    `json:"submitted_at"`
	Enrollment  *nmi.AddSubscriptionResponse `json:"enrollment,omitempty"`
	Sale        *nmi.SaleResponse            `json:"sale,omitempty"`
	Refusal     string                       `json:"refusal,omitempty"`
	// Resolution records operator evidence that supplied this step's outcome.
	Resolution map[string]any `json:"resolution,omitempty"`
}
type nmiUpgradeProgress struct {
	Successor *nmiUpgradeStep `json:"successor,omitempty"`
	Proration *nmiUpgradeStep `json:"proration,omitempty"`
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
	var p NMIUpgradePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return intents.Terminal("invalid upgrade payload: " + err.Error())
	}
	if p.NewSubscriptionID == uuid.Nil || p.NewPaymentID == uuid.Nil || p.OldSubscriptionID == uuid.Nil || p.PriceID == uuid.Nil || p.PlanID == "" || p.VaultID == "" || in.PspID == nil || !p.PeriodEnd.After(p.PeriodStart) {
		return intents.Terminal("incomplete frozen upgrade payload")
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
	save := func(key string, step *nmiUpgradeStep) error {
		return store.RecordProgress(ctx, in.ID, map[string]any{key: step})
	}
	if progress.Successor != nil && progress.Successor.Refusal != "" {
		return intents.TerminalWithEvidence(progress.Successor.Refusal, evidence())
	}
	if progress.Proration != nil && progress.Proration.Refusal != "" {
		return h.refusedProration(ctx, in, p, progress, evidence())
	}
	var client *nmi.NMIClient
	needsClient := progress.Successor == nil || (progress.Successor.Enrollment != nil && p.ProrationAmount > 0 && (progress.Proration == nil || progress.Proration.Sale == nil))
	if needsClient {
		client, err = h.Checkout.resolveNMIClient(ctx, nmiIntentClientName(p.PSP, in.Rail))
		if err != nil {
			return intents.Parked(err.Error())
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
		if p.RecurringAnchor != "" {
			credential = charge.RecurringReuse(p.RecurringAnchor)
		}
		order := "upgs-" + shortHash(in.IdempotencyKey)
		receipt, callErr := client.AddRecurringSubscription(ctx, nmi.RecurringPaymentData{CardUserData: p.Card, PlanID: p.PlanID, CustomerVaultID: p.VaultID, BillingID: p.BillingID, Amount: moneyutil.Cents(recurring), Currency: p.Currency, Email: p.Email, CustomerID: p.UserID, OrderID: order, PONumber: order, StartDate: p.StartDate, StoredCredential: nmidirect.StoredCredentialFor(credential)})
		if callErr != nil {
			if nmi.RequiresVerification(callErr) {
				return intents.Ambiguous("successor submission has no exact receipt: " + callErr.Error())
			}
			progress.Successor.Refusal = callErr.Error()
			if err = save("successor", progress.Successor); err != nil {
				return intents.Ambiguous("persist successor refusal: " + err.Error())
			}
			return intents.TerminalWithEvidence(callErr.Error(), evidence())
		}
		if receipt == nil || receipt.SubscriptionID == "" {
			return intents.Ambiguous("successor response omitted subscription identity")
		}
		progress.Successor.Enrollment = receipt
		if err = save("successor", progress.Successor); err != nil {
			return intents.Ambiguous("persist successor receipt: " + err.Error())
		}
	}
	if progress.Successor.Enrollment == nil {
		// The NMI roster exposes vault/plan, not the immutable upgrade order.
		// A unique matching roster row is not proof that THIS request created it.
		return intents.Ambiguous("successor submission needs an exact account-scoped provider receipt; roster similarity cannot authorize adoption or resend")
	}
	if p.ProrationAmount > 0 {
		order := "upg-" + shortHash(in.IdempotencyKey)
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
			if p.UnscheduledAnchor != "" {
				credential = charge.OneTimeReuse(p.UnscheduledAnchor)
			}
			receipt, callErr := client.RunSale(ctx, nmi.SaleParams{CustomerVaultID: p.VaultID, BillingID: p.BillingID, Amount: moneyutil.Cents(proration), Currency: p.Currency, OrderID: order, OrderDescription: "Upgrade: " + p.ProductName, StoredCredential: nmidirect.StoredCredentialFor(credential)})
			if callErr != nil {
				if nmi.RequiresVerification(callErr) {
					return intents.Ambiguous("proration submission has no exact receipt: " + callErr.Error())
				}
				progress.Proration.Refusal = callErr.Error()
				if err = save("proration", progress.Proration); err != nil {
					return intents.Ambiguous("persist proration refusal: " + err.Error())
				}
				return h.refusedProration(ctx, in, p, progress, evidence())
			}
			if receipt == nil || receipt.TransactionID == "" {
				return intents.Ambiguous("proration response omitted transaction identity")
			}
			progress.Proration.Sale = receipt
			if err = save("proration", progress.Proration); err != nil {
				return intents.Ambiguous("persist proration receipt: " + err.Error())
			}
		}
		if progress.Proration.Sale == nil {
			txn, found, err := client.FindSuccessfulSaleByOrderID(ctx, order)
			if err != nil {
				return intents.Ambiguous("read proration receipt: " + err.Error())
			}
			if !found {
				return intents.Ambiguous("submitted proration has no exact receipt; no resend")
			}
			progress.Proration.Sale = &nmi.SaleResponse{TransactionID: txn}
			if err = save("proration", progress.Proration); err != nil {
				return intents.Ambiguous("persist proration readback: " + err.Error())
			}
		}
	}
	if err = h.finalize(ctx, in, p, progress); err != nil {
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
	var p NMIUpgradePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
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
	if step.Refusal != "" || step.Enrollment != nil || step.Sale != nil {
		return intents.Outcome{}, intents.RejectResolution("%s step already has an outcome", resolution.Step)
	}
	client, err := h.Checkout.resolveNMIClient(ctx, nmiIntentClientName(p.PSP, in.Rail))
	if err != nil {
		return intents.Outcome{}, fmt.Errorf("resolve nmi client: %w", err)
	}
	ref := resolution.ProviderReference
	switch {
	case resolution.Step == "successor" && resolution.NotExecuted:
		step.Refusal = "provider confirmed the successor enrollment was not executed"
	case resolution.Step == "successor":
		if err := client.ConfirmLiveSubscription(ctx, ref, p.VaultID, p.PlanID); err != nil {
			return intents.Outcome{}, intents.RejectResolution("%v", err)
		}
		local, err := h.Checkout.SubscriptionService.GetByPSPSubscriptionID(ctx, in.Rail, ref)
		switch {
		case err == nil && local.ID != p.NewSubscriptionID:
			return intents.Outcome{}, intents.RejectResolution("subscription %s is already registered to local subscription %s", ref, local.ID)
		case err != nil && !db.IsNotFound(err):
			return intents.Outcome{}, err
		}
		step.Enrollment = &nmi.AddSubscriptionResponse{SubscriptionID: ref}
	case resolution.NotExecuted:
		if err := refuseContradictedNonExecution(ctx, client, "upg-"+shortHash(in.IdempotencyKey)); err != nil {
			return intents.Outcome{}, err
		}
		step.Refusal = "provider confirmed the proration sale was not executed"
	default:
		cents, err := moneyutil.NativeToRailMinorExact(p.Currency, p.ProrationAmount)
		if err != nil {
			return intents.Outcome{}, err
		}
		if err := client.ConfirmApprovedSale(ctx, ref, p.VaultID, cents, p.Currency); err != nil {
			return intents.Outcome{}, intents.RejectResolution("%v", err)
		}
		step.Sale = &nmi.SaleResponse{TransactionID: ref}
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
func (h *NMIUpgradeIntentHandler) refusedProration(ctx context.Context, in gen.OpenrailsRailIntent, p NMIUpgradePayload, progress nmiUpgradeProgress, evidence map[string]any) intents.Outcome {
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
		_, err := intents.NewStore(txDB).Enqueue(ctx, intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, SubscriptionID: &p.NewSubscriptionID, IntentType: intents.TypeNMIDeleteSubscription, Payload: intents.NMIDeletePayload{UserID: p.UserID, RailSubscriptionID: progress.Successor.Enrollment.SubscriptionID}, IdempotencyKey: intents.NMIDeleteIdempotencyKey(p.NewSubscriptionID), NextAttemptAt: p.PeriodStart, Origin: intents.OriginUser, OriginReason: "cancel unpaid upgrade successor after definitive proration refusal"})
		return err
	})
	if err != nil {
		return intents.AmbiguousWithEvidence("proration refused; durable successor cancellation pending: "+err.Error(), evidence)
	}
	return intents.TerminalWithEvidence("proration refused; unpaid successor queued for cancellation: "+progress.Proration.Refusal, evidence)
}

func (h *NMIUpgradeIntentHandler) finalize(ctx context.Context, in gen.OpenrailsRailIntent, p NMIUpgradePayload, progress nmiUpgradeProgress) error {
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
	if progress.Proration != nil && progress.Proration.Sale != nil {
		payment = &models.Payment{ID: p.NewPaymentID, CustomerID: customer, PriceID: p.PriceID, SubscriptionID: &next.ID, Rail: models.Rail(in.Rail), PspID: in.PspID, TransactionID: progress.Proration.Sale.TransactionID, Amount: p.ProrationAmount, ListAmount: p.RecurringAmount, Currency: p.Currency, Status: "completed", MoneyMovement: models.MoneyMovementRail, PurchasedAt: p.PeriodStart, EntitlementsSpecSnapshot: p.Entitlements, Metadata: map[string]any{"upgrade_intent_id": in.ID.String()}}
	}
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := database.NewWithPgxTx(tx)
		if err := h.Checkout.Lifecycle.CompleteUpgradeTx(ctx, txDB, models.Subscription{ID: p.OldSubscriptionID, PriceID: p.OldPriceID, RailSubscriptionID: p.OldProviderSubscriptionID}, next, payment); err != nil {
			return err
		}
		for agreement, ref := range map[string]string{"recurring": progress.Successor.Enrollment.TransactionID, "unscheduled": func() string {
			if payment != nil {
				return payment.TransactionID
			}
			return ""
		}()} {
			if ref != "" {
				if _, err := txDB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID, Agreement: agreement, Ref: ref}); err != nil {
					return err
				}
			}
		}
		_, err := intents.NewStore(txDB).Enqueue(ctx, intents.EnqueueParams{MerchantID: in.MerchantID, Provider: in.Rail, PspID: *in.PspID, SubscriptionID: &p.OldSubscriptionID, IntentType: intents.TypeNMIDeleteSubscription, Payload: intents.NMIDeletePayload{UserID: p.UserID, RailSubscriptionID: p.OldProviderSubscriptionID}, IdempotencyKey: intents.NMIDeleteIdempotencyKey(p.OldSubscriptionID), NextAttemptAt: p.PeriodStart, Origin: intents.OriginUser, OriginReason: "cancel predecessor after durable tier upgrade"})
		return err
	})
}

func (s *CheckoutService) resumeUpgrade(ctx context.Context, prior gen.OpenrailsRailIntent) (*CheckoutResponse, error) {
	if prior.Status == intents.StatusSucceeded || prior.Status == intents.StatusFailedTerminal {
		return upgradeResponse(prior)
	}
	if s.Intents == nil {
		return nil, ErrCheckoutProcessing
	}
	var p NMIUpgradePayload
	if err := json.Unmarshal(prior.Payload, &p); err != nil {
		return nil, err
	}
	row, err := s.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{MerchantID: prior.MerchantID, Provider: prior.Rail, PspID: *prior.PspID, SubscriptionID: prior.SubscriptionID, PriceID: prior.PriceID, IntentType: TypeNMIUpgrade, Payload: p, IdempotencyKey: prior.IdempotencyKey, NextAttemptAt: s.now(), Origin: intents.OriginUser, OriginReason: "resume tier upgrade"})
	if err != nil {
		return nil, err
	}
	return upgradeResponse(row)
}
func upgradeResponse(in gen.OpenrailsRailIntent) (*CheckoutResponse, error) {
	switch in.Status {
	case intents.StatusSucceeded:
		var result struct {
			SubscriptionID string `json:"subscription_id"`
			TransactionID  string `json:"transaction_id"`
			Message        string `json:"message"`
		}
		if err := json.Unmarshal(in.ResultEvidence, &result); err != nil {
			return nil, err
		}
		id, err := uuid.Parse(result.SubscriptionID)
		if err != nil {
			return nil, err
		}
		return &CheckoutResponse{Status: "success", Action: "upgrade", SubscriptionID: &id, TransactionID: result.TransactionID, Message: result.Message}, nil
	case intents.StatusFailedTerminal:
		if in.LastFailureReason != nil {
			return nil, errors.New(*in.LastFailureReason)
		}
		return nil, fmt.Errorf("upgrade refused")
	default:
		return nil, ErrCheckoutProcessing
	}
}

// Replays precede mutable catalog and predecessor-status admission. Once the
// upgrade commits, its predecessor is cancelled and its price may be archived.
func (s *CheckoutService) replayTierUpgrade(ctx context.Context, req *TierChangeRequest, user *UserIdentity) (*TierChangeResponse, bool, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || s.SubscriptionService == nil {
		return nil, false, nil
	}
	store := intents.NewStore(s.SubscriptionService.Database())
	in, err := store.GetByIdempotencyKey(ctx, NMIUpgradeIdempotencyKey(req.IdempotencyKey))
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var p NMIUpgradePayload
	if err = json.Unmarshal(in.Payload, &p); err != nil {
		return nil, true, err
	}
	if user == nil || p.UserID != user.ID {
		return nil, true, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "upgrade not found"}
	}
	price := strings.TrimSpace(req.PriceID)
	if (req.SubscriptionID != uuid.Nil && req.SubscriptionID != p.OldSubscriptionID) || (price != p.RequestedPrice && price != p.PriceID.String() && price != api.FormatPriceID(p.PriceID)) {
		return nil, true, &TierChangeError{HTTPStatus: http.StatusConflict, Message: "upgrade idempotency key belongs to a different request"}
	}
	if _, err = s.resumeUpgrade(ctx, in); err != nil {
		return nil, true, err
	}
	in, err = store.Get(ctx, in.ID)
	if err != nil {
		return nil, true, err
	}
	response, err := s.upgradeTierResponse(in)
	return response, true, err
}

func (s *CheckoutService) upgradeTierResponse(in gen.OpenrailsRailIntent) (*TierChangeResponse, error) {
	response, err := upgradeResponse(in)
	if err != nil {
		return nil, err
	}
	var p NMIUpgradePayload
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return nil, err
	}
	result := s.mapCheckoutToTierChangeResponse(response, &models.Price{ID: p.PriceID}, "upgrade")
	result.Payment.Rail = in.Rail
	result.Currency = p.Currency
	result.AmountDueNow = p.ProrationAmount
	result.NextChargeAmount = p.RecurringAmount
	result.NextChargeDate = &p.PeriodEnd
	return result, nil
}
