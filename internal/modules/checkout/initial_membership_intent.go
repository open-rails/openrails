package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	hscharge "github.com/open-rails/openrails/internal/modules/payments/rails/hyperswitch"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const TypeInitialMembership = subscriptions.TypeInitialMembership

type InitialMembershipPayload = subscriptions.InitialMembershipPayload

func InitialMembershipIdempotencyKey(key string) string {
	return TypeInitialMembership + ":" + strings.TrimSpace(key)
}
func decodeInitialMembershipPayload(in gen.OpenrailsRailIntent) (InitialMembershipPayload, error) {
	return subscriptions.DecodeInitialMembershipPayload(in)
}

type InitialMembershipIntentHandler struct {
	Checkout *CheckoutService
	Resolver intents.NMIClientResolver
	Policy   intents.BackoffPolicy
}

func NewInitialMembershipIntentHandler(s *CheckoutService, resolvers ...intents.NMIClientResolver) *InitialMembershipIntentHandler {
	h := &InitialMembershipIntentHandler{Checkout: s, Policy: intents.DefaultBackoff}
	if len(resolvers) > 0 {
		h.Resolver = resolvers[0]
	}
	return h
}
func (h *InitialMembershipIntentHandler) Type() string { return TypeInitialMembership }
func (h *InitialMembershipIntentHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}
func (h *InitialMembershipIntentHandler) PrunePolicy() (bool, bool)    { return true, true }
func (h *InitialMembershipIntentHandler) CommitsTerminalOutcome() bool { return true }
func (h *InitialMembershipIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *InitialMembershipIntentHandler) database() *db.DB {
	if h.Checkout == nil || h.Checkout.SubscriptionService == nil {
		return nil
	}
	return h.Checkout.SubscriptionService.Database()
}
func (h *InitialMembershipIntentHandler) client(ctx context.Context, in gen.OpenrailsRailIntent, p InitialMembershipPayload) (*nmi.NMIClient, error) {
	var client *nmi.NMIClient
	var err error
	if p.Terms.CollectionPolicy == models.CollectionPolicyEngine {
		if h.Resolver == nil {
			return nil, errors.New("initial membership account resolver unavailable")
		}
		var found bool
		client, found, err = h.Resolver.ResolveNMIClient(ctx, in.MerchantID, in.PspID)
		if err == nil && (!found || client == nil) {
			return nil, errors.New("initial membership account unavailable")
		}
	} else {
		client, err = h.Checkout.resolveNMIClient(db.WithPSPID(ctx, *in.PspID), p.PSP)
	}
	if err != nil {
		return nil, err
	}
	owner, account := client.AccountIdentity()
	if owner != in.MerchantID || account != *in.PspID {
		return nil, errors.New("enrollment client differs from accepted account")
	}
	return client, nil
}

func (h *InitialMembershipIntentHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	if h.database() == nil || h.Checkout.Lifecycle == nil {
		return intents.Parked("initial membership services unavailable")
	}
	current, err := intents.NewStore(h.database()).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	var progress map[string]any
	if len(current.ResultEvidence) > 0 && json.Unmarshal(current.ResultEvidence, &progress) != nil {
		return intents.Ambiguous("invalid enrollment progress")
	}
	if value, present := progress["initial_submitted"]; present && value != true {
		return intents.Ambiguous("invalid initial submission fence")
	}
	if _, refused, err := intents.LoadInitialMembershipRefusal(current); err != nil {
		return intents.Ambiguous(err.Error())
	} else if refused {
		return h.Verify(ctx, current)
	}
	if current.Status == intents.StatusSucceeded || current.Status == intents.StatusFailedTerminal {
		return h.Verify(ctx, current)
	}
	if progress["initial_submitted"] == true {
		if current.Rail == "stripe" {
			return h.executeStripeInitialDecline(ctx, current)
		}
		return h.Verify(ctx, current)
	}
	in = current
	p, err := decodeInitialMembershipPayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if p.Terms.CollectionPolicy == models.CollectionPolicyEngine && h.Checkout.Config != nil && h.Checkout.Config.EngineAdmissionHold {
		return intents.Parked("engine payment admission is held")
	}
	if in.Rail == "stripe" {
		return h.executeStripeInitial(ctx, in, p)
	}
	client, err := h.client(ctx, in, p)
	if err != nil {
		return intents.Parked(err.Error())
	}
	if client.ReadOnly {
		return intents.Parked("native enrollment account is read-only")
	}
	var proxy *hscharge.Charger
	if p.HyperSwitch != nil {
		proxy, err = h.hyperSwitchCharger(ctx, in, p, client)
		if err != nil {
			return intents.Parked(err.Error())
		}
	} else if _, err = client.ReadSingleCardVaultBilling(ctx, p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef); err != nil {
		return intents.Parked("enrollment instrument readback unavailable or unqualified")
	}
	proof, submitted, err := h.fenceInitialMembership(ctx, in, p)
	if err != nil {
		return intents.Ambiguous("enrollment could not retain its submission fence: " + err.Error())
	}
	if !submitted {
		return h.Verify(ctx, in)
	}
	minor, err := moneyutil.NativeToRailMinorExact(p.Terms.Currency, p.Terms.Amount)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if proxy != nil {
		mode := charge.InitialRecurring()
		if p.Instrument.StoredCredentialRecurringRef != "" {
			mode = charge.RecurringReuse(p.Instrument.StoredCredentialRecurringRef)
		}
		result, refusal, err := proxy.ChargeInitialRecurring(ctx, charge.Request{Instrument: charge.Instrument{Rail: "nmi", CustomerRef: p.Instrument.RailCustomerRef, MethodRef: p.Instrument.RailMethodRef}, AmountMinor: minor, Currency: p.Terms.Currency, OrderRef: intents.NMIEnrollmentOrder(in), Context: mode})
		if errors.Is(err, charge.ErrNotDispatched) {
			return h.completeInitialNonexecution(ctx, in, proof)
		}
		if err != nil {
			return intents.Ambiguous("initial membership charge requires provider verification")
		}
		if refusal != nil {
			if err := intents.NewStore(h.database()).RetainInitialMembershipDecline(ctx, in, refusal); err != nil {
				return intents.Ambiguous(err.Error())
			}
		} else {
			if result.TransactionID == "" || result.Declined {
				return intents.Ambiguous("initial membership has no qualified charge candidate")
			}
			if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, map[string]any{"transaction_id": result.TransactionID}); err != nil {
				return intents.Ambiguous(err.Error())
			}
		}
		return h.Verify(ctx, in)
	}
	if p.Terms.CollectionPolicy == models.CollectionPolicyEngine {
		mode := charge.InitialRecurring()
		if p.Instrument.StoredCredentialRecurringRef != "" {
			mode = charge.RecurringReuse(p.Instrument.StoredCredentialRecurringRef)
		}
		result, refusal, err := nmidirect.New(client).ChargeInitialRecurring(ctx, charge.Request{Instrument: charge.Instrument{Rail: "nmi", CustomerRef: p.Instrument.RailCustomerRef, MethodRef: p.Instrument.RailMethodRef}, AmountMinor: minor, Currency: p.Terms.Currency, OrderRef: intents.NMIEnrollmentOrder(in), Context: mode})
		if errors.Is(err, charge.ErrNotDispatched) {
			return h.completeInitialNonexecution(ctx, in, proof)
		}
		if err != nil {
			return intents.Ambiguous("initial native charge requires verification")
		}
		if refusal != nil {
			if err := intents.NewStore(h.database()).RetainInitialMembershipDecline(ctx, in, refusal); err != nil {
				return intents.Ambiguous(err.Error())
			}
		} else {
			if result.TransactionID == "" || result.Declined {
				return intents.Ambiguous("initial native payment has no qualified candidate")
			}
			if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, map[string]any{"transaction_id": result.TransactionID}); err != nil {
				return intents.Ambiguous(err.Error())
			}
		}
		return h.Verify(ctx, in)
	}
	var credential *nmi.StoredCredential
	if p.Terms.Amount > 0 {
		mode := charge.InitialRecurring()
		if p.Instrument.StoredCredentialRecurringRef != "" {
			mode = charge.RecurringReuse(p.Instrument.StoredCredentialRecurringRef)
		}
		credential = nmidirect.StoredCredentialFor(mode)
	}
	order := intents.NMIEnrollmentOrder(in)
	response, callErr := client.AddRecurringSubscription(ctx, nmi.RecurringPaymentData{ScheduleOnly: p.Terms.Amount == 0, PlanID: p.NativeSchedule.PlanID, CustomerVaultID: p.Instrument.RailCustomerRef, BillingID: p.Instrument.RailMethodRef, Amount: minor, Currency: p.Terms.Currency, Email: p.Email, OrderID: order, PONumber: order, StartDate: p.NativeSchedule.StartDate, StoredCredential: credential, CardUserData: nmi.CardUserData{FirstName: p.NativeSchedule.Card.FirstName, LastName: p.NativeSchedule.Card.LastName, Address1: p.NativeSchedule.Card.Address1, City: p.NativeSchedule.Card.City, State: p.NativeSchedule.Card.State, Zip: p.NativeSchedule.Card.Zip, Country: p.NativeSchedule.Card.Country}})
	if callErr != nil {
		if nmi.RequiresVerification(callErr) {
			return intents.Ambiguous("native enrollment outcome requires exact provider verification")
		}
		var refusal *nmi.CustomerVaultError
		if !errors.As(callErr, &refusal) {
			return intents.Ambiguous("native enrollment rejection has no qualified refusal proof")
		}
		if err := intents.NewStore(h.database()).RetainInitialMembershipDecline(ctx, in, refusal); err != nil {
			return intents.Ambiguous("native enrollment refusal could not be qualified: " + err.Error())
		}
		return h.Verify(ctx, in)
	}
	if response == nil {
		return intents.Ambiguous("native enrollment returned no receipt candidates")
	}
	if err := intents.NewStore(h.database()).RecordProgress(ctx, in.ID, map[string]any{"provider_subscription_id": response.SubscriptionID, "transaction_id": response.TransactionID}); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.Verify(ctx, in)
}

func (h *InitialMembershipIntentHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) intents.Outcome {
	if h.database() == nil || h.Checkout.Lifecycle == nil {
		return intents.Ambiguous("initial membership recovery unavailable")
	}
	current, err := intents.NewStore(h.database()).Get(ctx, in.ID)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	in = current
	p, err := decodeInitialMembershipPayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	schedule, scheduled, err := intents.LoadNMIEnrollmentReceipt(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	_, paid, err := intents.LoadCollectedReceipt(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if (p.NativeSchedule != nil && scheduled || p.NativeSchedule == nil && !scheduled) && (p.Terms.Amount == 0 || paid) {
		return h.complete(ctx, in, intents.Succeeded(nil))
	}
	var progress map[string]any
	if len(in.ResultEvidence) > 0 && json.Unmarshal(in.ResultEvidence, &progress) != nil {
		return intents.Ambiguous("invalid enrollment progress")
	}
	refusal, refused, err := intents.LoadInitialMembershipRefusal(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	if refused {
		return h.complete(ctx, in, refusal.Outcome())
	}
	if value, present := progress["initial_submitted"]; present && value != true {
		return intents.Ambiguous("invalid initial submission fence")
	}
	if progress["initial_submitted"] != true {
		return intents.Retryable("unsubmitted payment awaits gated execution")
	}
	if in.Rail == "stripe" {
		return h.verifyStripeInitial(ctx, in, p)
	}
	client, err := h.client(ctx, in, p)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	store := intents.NewStore(h.database())
	var paymentErr error
	if p.Terms.Amount > 0 && !paid {
		paymentErr = h.retainInitialPayment(ctx, in, client)
	}
	if p.NativeSchedule == nil {
		if paymentErr != nil {
			return intents.Ambiguous(paymentErr.Error())
		}
		return h.complete(ctx, in, intents.Succeeded(nil))
	}
	if !scheduled {
		refs := []string{intents.EvidenceString(in, "provider_subscription_id")}
		if refs[0] == "" {
			roster, err := scanRemoteSubscriptions(db.WithPSPID(ctx, *in.PspID), h.Checkout.SubscriptionService, client, "nmi", p.Instrument.RailCustomerRef, p.NativeSchedule.PlanID, intents.NMIEnrollmentOrder(in))
			if err != nil {
				return intents.Ambiguous(err.Error())
			}
			refs = append(roster.registered, roster.unregistered...)
		}
		matches := 0
		for _, ref := range refs {
			if ref == "" {
				continue
			}
			candidate, found, err := intents.ReadNMIEnrollmentReceipt(ctx, in, upgradeReceiptResolver{client}, ref)
			if err != nil || !found {
				continue
			}
			matches++
			schedule = candidate
		}
		if matches != 1 {
			return intents.AmbiguousWithEvidence("native enrollment has no unique qualified schedule receipt", map[string]any{"candidate_subscription_ids": refs})
		}
		schedule, err = store.RetainNMIEnrollmentReceipt(ctx, in, schedule)
		if err != nil {
			return intents.Ambiguous(err.Error())
		}
	}
	if paymentErr != nil {
		return intents.Ambiguous(paymentErr.Error())
	}
	return h.complete(ctx, in, intents.Succeeded(map[string]any{"provider_subscription_id": schedule.SubscriptionID()}))
}

func (h *InitialMembershipIntentHandler) retainInitialPayment(ctx context.Context, in gen.OpenrailsRailIntent, client *nmi.NMIClient) error {
	var err error
	ref := intents.EvidenceString(in, "transaction_id")
	if ref == "" {
		var found bool
		ref, found, err = client.FindSuccessfulSaleByOrderID(ctx, intents.NMIEnrollmentOrder(in))
		if err != nil || !found {
			return errors.New("initial payment has no qualified transaction candidate")
		}
	}
	receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, ref)
	if err != nil || !found {
		return errors.New("initial payment does not match accepted money and instrument")
	}
	if _, err = intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt); err != nil {
		return err
	}
	return nil
}

func (h *InitialMembershipIntentHandler) Resolve(ctx context.Context, in gen.OpenrailsRailIntent, resolution intents.Resolution) (intents.Outcome, error) {
	if resolution.Step != "" {
		return intents.Outcome{}, intents.RejectResolution("initial enrollment has no steps")
	}
	if _, err := decodeInitialMembershipPayload(in); err != nil {
		return intents.Outcome{}, err
	}
	if resolution.NotExecuted {
		current, err := intents.NewStore(h.database()).Get(ctx, in.ID)
		if err != nil {
			return intents.Outcome{}, err
		}
		var progress map[string]any
		if len(current.ResultEvidence) > 0 && json.Unmarshal(current.ResultEvidence, &progress) != nil {
			return intents.Outcome{}, errors.New("invalid enrollment progress")
		}
		if progress["initial_submitted"] == true {
			return intents.Outcome{}, intents.RejectResolution("submitted NMI enrollment has no positive nonexecution proof contract")
		}
		return h.complete(ctx, current, intents.TerminalWithEvidence("enrollment was never submitted", map[string]any{"not_executed": true})), nil
	}
	if strings.TrimSpace(resolution.ProviderReference) == "" {
		return intents.Outcome{}, intents.RejectResolution("exact schedule reference required")
	}
	p, _ := decodeInitialMembershipPayload(in)
	if in.Rail == "stripe" {
		service, err := h.stripeEngineService(ctx, in)
		if err != nil {
			return intents.Outcome{}, err
		}
		receipt, found, err := intents.ReadStripeEngineReceipt(ctx, in, service, resolution.ProviderReference)
		if err != nil || !found {
			return intents.Outcome{}, intents.RejectResolution("payment does not prove accepted membership")
		}
		if _, err := intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt); err != nil {
			return intents.Outcome{}, err
		}
		return h.Verify(ctx, in), nil
	}
	client, err := h.client(ctx, in, p)
	if err != nil {
		return intents.Outcome{}, err
	}
	if p.NativeSchedule == nil {
		receipt, found, err := intents.ReadNMICollectionReceipt(ctx, in, upgradeReceiptResolver{client}, resolution.ProviderReference)
		if err != nil || !found {
			return intents.Outcome{}, intents.RejectResolution("payment does not prove accepted membership")
		}
		if _, err := intents.NewStore(h.database()).RetainCollectedReceipt(ctx, in, receipt); err != nil {
			return intents.Outcome{}, err
		}
		return h.Verify(ctx, in), nil
	}
	schedule, found, err := intents.ReadNMIEnrollmentReceipt(ctx, in, upgradeReceiptResolver{client}, resolution.ProviderReference)
	if err != nil || !found {
		return intents.Outcome{}, intents.RejectResolution("schedule does not prove accepted enrollment")
	}
	if _, err = intents.NewStore(h.database()).RetainNMIEnrollmentReceipt(ctx, in, schedule); err != nil {
		return intents.Outcome{}, err
	}
	return h.Verify(ctx, in), nil
}

func (h *InitialMembershipIntentHandler) complete(ctx context.Context, in gen.OpenrailsRailIntent, outcome intents.Outcome) intents.Outcome {
	p, err := decodeInitialMembershipPayload(in)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	ctx, cancel := intents.LedgerWriteContext(ctx)
	defer cancel()
	ctx = db.WithPSPID(ctx, *in.PspID)
	err = h.database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.database().NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.Terms.CustomerID}); err != nil {
			return err
		}
		current, err := d.Gen(ctx).LockRailIntentForInitialEnrollmentCompletion(ctx, gen.LockRailIntentForInitialEnrollmentCompletionParams{MerchantID: in.MerchantID, ID: in.ID})
		if err != nil {
			return err
		}
		if current.PspID == nil || *current.PspID != *in.PspID || current.IdempotencyKey != in.IdempotencyKey || !bytes.Equal(current.Payload, in.Payload) {
			return errors.New("initial completion differs from accepted operation")
		}
		schedule, scheduled, err := intents.LoadNMIEnrollmentReceipt(current)
		if err != nil {
			return err
		}
		receipt, paid, err := intents.LoadCollectedReceipt(current)
		if err != nil {
			return err
		}
		refusal, refused, err := intents.LoadInitialMembershipRefusal(current)
		if err != nil {
			return err
		}
		success := outcome.Class == intents.OutcomeSucceeded
		if !success && !refused && outcome.Evidence["not_executed"] == true {
			if err := intents.NewStore(d).RetainUnsubmittedInitialMembership(ctx, current); err != nil {
				return err
			}
			current, err = intents.NewStore(d).Get(ctx, current.ID)
			if err != nil {
				return err
			}
			refusal, refused, err = intents.LoadInitialMembershipRefusal(current)
			if err != nil {
				return err
			}
		}
		if success && refused || !success && !refused {
			return errors.New("initial terminal decision has no matching qualified custody")
		}
		if !success {
			outcome = refusal.Outcome()
		}

		if success && ((p.NativeSchedule != nil) != scheduled || (p.Terms.Amount > 0) != paid) {
			return errors.New("initial completion lacks exact required schedule/payment receipts")
		}
		if !success && (scheduled || paid) {
			return errors.New("qualified provider effects cannot be refused")
		}
		var evidence map[string]any
		if len(current.ResultEvidence) > 0 && json.Unmarshal(current.ResultEvidence, &evidence) != nil {
			return errors.New("invalid initial completion evidence")
		}
		if evidence == nil {
			evidence = map[string]any{}
		}
		// Generic diagnostic progress is not financial authority. Only the
		// validated private refusal above may populate terminal refusal fields.
		for _, key := range []string{"authentication_required", "declined", "not_executed", "request_refused", "response_code", "localization_id"} {
			delete(evidence, key)
		}

		status := intents.StatusFailedTerminal
		if success {
			status = intents.StatusSucceeded
		}
		if current.Status == intents.StatusSucceeded || current.Status == intents.StatusFailedTerminal {
			if current.Status != status {
				return errors.New("initial terminal replay conflicts")
			}
			outcome.Evidence = saleResultEvidence(evidence)
			return h.projectInitialMembershipSession(ctx, d, current, p, success)

		}
		if success {
			if p.Upgrade() {
				if err := h.Checkout.Lifecycle.SupersedeForUpgradeTx(ctx, d, p.Terms, models.Rail(in.Rail)); err != nil {
					return err
				}
			}
			providerSub := schedule.SubscriptionID()
			transaction := ""
			if paid {
				transaction = receipt.TransactionID()
			}
			metadata := map[string]any{"order_id": intents.NMIEnrollmentOrder(in), "provider_transaction_id": transaction}
			if reversal := receipt.ReversalKind(); reversal != "" {
				metadata["initial_payment_reversal"] = reversal
			}
			if p.DelayedStart() != nil {
				metadata["delayed_start"] = p.DelayedStart().UTC().Format(time.RFC3339Nano)
			}
			var email *string
			if p.Email != "" {
				email = &p.Email
			}
			if _, _, err := h.Checkout.Lifecycle.CreateMembershipTx(ctx, d, &subscriptions.CreateMembershipParams{Prepared: &p.Terms, PaymentCustodian: p.Instrument.Custodian, InitialPaymentReversal: receipt.ReversalKind(), UserID: p.Terms.CustomerID.String(), PriceID: p.Terms.PriceID, Rail: models.Rail(in.Rail), RailSubscriptionID: &providerSub, UserEmail: email, TransactionID: transaction, Amount: p.Terms.Amount, AmountProvided: true, Currency: p.Terms.Currency, PurchasedAt: &p.Terms.AcceptedAt, PaymentMetadata: metadata}); err != nil {
				return err
			}
			if paid {
				agreementRef := transaction
				if in.Rail == "stripe" {
					agreementRef = receipt.StripeEnginePaymentIntentID()
				}
				if _, err := d.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{MerchantID: in.MerchantID, ID: p.Terms.PaymentMethodID, Agreement: "recurring", Ref: agreementRef}); err != nil {
					return err
				}
			}
			evidence["subscription_id"], evidence["provider_subscription_id"], evidence["transaction_id"], evidence["status"], evidence["message"] = p.Terms.SubscriptionID.String(), providerSub, transaction, "success", "Subscription created successfully"
			if p.Upgrade() {
				evidence["message"] = "Upgraded to " + p.Terms.ProductName
			}
			if receipt.ReversalKind() != "" {
				evidence["message"] = "Payment recorded; enrollment canceled after provider reversal"
			}
			if p.Terms.Pending {
				evidence["status"] = "pending"
				evidence["message"] = "Subscription scheduled for its accepted start date"
			}
			if p.DelayedStart() != nil {
				evidence["delayed_start"] = p.DelayedStart().UTC().Format(time.RFC3339Nano)
			}
		} else {
			if outcome.Evidence["declined"] == true && p.Terms.Amount > 0 {
				code := fmt.Sprint(outcome.Evidence["response_code"])
				if in.Rail == "stripe" {
					code = fmt.Sprint(outcome.Evidence["failure_code"])
				}
				reason := payments.NormalizeFailureReason(in.Rail, code)
				kind, token := payments.AttemptInitial, charge.TokenTypePSPToken
				if p.Instrument.CustodianHeld() {
					token = charge.TokenTypePANViaProxy
				}
				if err := payments.NewPaymentService(d, h.Checkout.Clock()).Create(ctx, &models.Payment{ID: p.Terms.PaymentID, CustomerID: p.Terms.CustomerID, PriceID: p.Terms.PriceID, PspID: in.PspID, Rail: models.Rail(in.Rail), TransactionID: in.Rail + "_sub_declined:" + in.ID.String(), Amount: p.Terms.Amount, ListAmount: p.Terms.RecurringAmount, Currency: p.Terms.Currency, Status: payments.PaymentStatusFailedValue, AttemptKind: &kind, TokenType: &token, FailureCode: &code, FailureReason: &reason, MoneyMovement: models.MoneyMovementNone, PurchasedAt: p.Terms.AcceptedAt, CreatedAt: p.Terms.AcceptedAt}); err != nil {
					return err
				}
			}
			for key, value := range outcome.Evidence {
				evidence[key] = value
			}
		}
		if record := intents.OperatorResolutionRecord(ctx); record != nil {
			evidence["operator_resolution"] = record
		}
		if err := h.projectInitialMembershipSession(ctx, d, current, p, success); err != nil {
			return err
		}
		raw, err := json.Marshal(evidence)
		if err != nil {
			return err
		}
		reason := outcome.Reason
		n, err := d.Gen(ctx).CompleteInitialEnrollmentOutcome(ctx, gen.CompleteInitialEnrollmentOutcomeParams{MerchantID: in.MerchantID, ID: in.ID, Status: status, Evidence: raw, Reason: &reason, Now: h.Checkout.now().UTC()})
		if err != nil {
			return err
		}
		if n != 1 {
			return errors.New("initial terminal decision did not commit")
		}
		outcome.Evidence = saleResultEvidence(evidence)
		return nil
	})
	if err != nil {
		return intents.Ambiguous("initial receipts retained; local completion pending: " + err.Error())
	}
	return outcome
}

// railSubscriptionReader is the local-lookup surface the roster scan needs
// (satisfied by *subscriptions.SubscriptionService).
type railSubscriptionReader interface {
	GetByPSPSubscriptionID(ctx context.Context, provider, railSubscriptionID string) (*models.Subscription, error)
}

type rosterMatches struct {
	// registered rows carry THIS operation's order id locally: exact evidence.
	registered []string
	// unregistered rows share only vault and plan: operator candidates.
	unregistered []string
}

// scanRemoteSubscriptions scans the NMI recurring roster for subscriptions on
// (vault, plan) and classifies them by local evidence.
func scanRemoteSubscriptions(ctx context.Context, subs railSubscriptionReader, client *nmi.NMIClient, provider, railCustomerRef, planID, orderID string) (rosterMatches, error) {
	var out rosterMatches
	cursor := ""
	for {
		page, perr := client.ListSubscriptionsPage(ctx, cursor, 0)
		if perr != nil {
			return out, fmt.Errorf("subscription roster read failed: %w", perr)
		}
		for _, sub := range page.Subscriptions {
			if strings.TrimSpace(sub.CustomerVaultID) != strings.TrimSpace(railCustomerRef) {
				continue
			}
			if sub.Plan == nil || strings.TrimSpace(sub.Plan.ID) != strings.TrimSpace(planID) {
				continue
			}
			local, lerr := subs.GetByPSPSubscriptionID(ctx, provider, sub.ID)
			if lerr != nil {
				if !db.IsNotFound(lerr) {
					return out, fmt.Errorf("local subscription lookup failed: %w", lerr)
				}
				out.unregistered = append(out.unregistered, sub.ID)
				continue
			}
			if orderID != "" && subscriptionMetadataString(local.Metadata, "order_id") == orderID {
				out.registered = append(out.registered, sub.ID)
			}
		}
		next := string(page.NextCursor)
		if !page.HasMore || next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// nmiIntentClientName picks the client-resolution name for an NMI intent: the
// payload's pinned PSP, else the intent's rail (active account).
func nmiIntentClientName(psp, rail string) string {
	if name := strings.ToLower(strings.TrimSpace(psp)); name != "" {
		return name
	}
	return strings.ToLower(strings.TrimSpace(rail))
}

// subscriptionMetadataString reads one string key off a subscription's raw
// JSON metadata.
func subscriptionMetadataString(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// finalize registers the confirmed remote subscription locally through the
// standard registration path (idempotent: existing rows are activated /
// answered, not duplicated) and completes the request-level idempotency
// record so client replays get the cached response.

func (h *InitialMembershipIntentHandler) fenceInitialMembership(ctx context.Context, in gen.OpenrailsRailIntent, p InitialMembershipPayload) (intents.InitialMembershipNonexecutionProof, bool, error) {
	var proof intents.InitialMembershipNonexecutionProof
	if p.Terms.CollectionPolicy == models.CollectionPolicyEngine && h.Checkout.Config != nil && h.Checkout.Config.EngineAdmissionHold {
		return proof, false, errors.New("engine payment admission is held")
	}
	submitted := false
	err := h.database().MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.database().NewWithPgxTx(tx)
		if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: in.MerchantID, ID: p.Terms.CustomerID}); err != nil {
			return err
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: in.MerchantID, ID: p.Terms.PaymentMethodID})
		if err != nil {
			return err
		}
		if method.CustomerID != p.Terms.CustomerID || method.ParkReason != "" {
			return charge.ErrInstrumentChanged
		}
		if err = p.Instrument.Matches(method, charge.AgreementRecurring); err != nil {
			return err
		}
		if p.HyperSwitch != nil {
			binding, err := charge.FreezeHyperSwitchBinding(ctx, d.Gen(ctx), method, h.Checkout.Config.HyperSwitch.APIBaseURL)
			if err != nil {
				return err
			}
			if binding != *p.HyperSwitch {
				return charge.ErrInstrumentChanged
			}
		}
		proof, submitted, err = intents.NewStore(d).BeginInitialMembershipPayment(ctx, in)
		return err
	})
	return proof, submitted, err
}

func (h *InitialMembershipIntentHandler) projectInitialMembershipSession(ctx context.Context, d *db.DB, in gen.OpenrailsRailIntent, p InitialMembershipPayload, success bool) error {
	if p.CheckoutSessionID == nil {
		return nil
	}
	id := *p.CheckoutSessionID
	params := gen.CompleteInitialMembershipSessionParams{ID: id, MerchantID: in.MerchantID, CustomerID: p.Terms.CustomerID, PriceID: p.Terms.PriceID, PspID: p.Terms.PSPID, Rail: in.Rail, Status: "failed", Now: h.Checkout.now().UTC()}
	if success {
		params.Status = "succeeded"
		params.SubscriptionID = &p.Terms.SubscriptionID
		if p.Terms.Amount > 0 {
			receipt, paid, err := intents.LoadCollectedReceipt(in)
			if err != nil {
				return err
			}
			if !paid {
				return errors.New("initial checkout projection has no paid receipt")
			}
			params.PaymentID = &p.Terms.PaymentID
			transaction := receipt.TransactionID()
			params.TransactionID = &transaction
		}
	}
	rows, err := d.Gen(ctx).CompleteInitialMembershipSession(ctx, params)
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("initial checkout projection differs from accepted session")
	}
	return nil
}

func (h *InitialMembershipIntentHandler) completeInitialNonexecution(ctx context.Context, in gen.OpenrailsRailIntent, proof intents.InitialMembershipNonexecutionProof) intents.Outcome {
	if err := intents.NewStore(h.database()).RetainInitialMembershipNonexecution(ctx, in, proof); err != nil {
		return intents.Ambiguous(err.Error())
	}
	return h.Verify(ctx, in)
}
