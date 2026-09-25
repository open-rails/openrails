package intents

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const rebillSubmittedAt = "submitted_at"

var errRebillSuperseded = errors.New("accepted rebill is no longer applicable")

type ManualRebillHandler struct {
	DB          *db.DB
	Config      *config.Config
	Resolver    NMIClientResolver
	Clock       clockwork.Clock
	Policy      BackoffPolicy
	DeferDelete subscriptions.DeferredDeleteScheduler
}

func NewManualRebillHandler(d *db.DB, cfg *config.Config, resolver NMIClientResolver, clock clockwork.Clock) *ManualRebillHandler {
	return &ManualRebillHandler{DB: d, Config: cfg, Resolver: resolver, Clock: timeutil.FirstClock(clock), Policy: DefaultBackoff}
}
func (h *ManualRebillHandler) Type() string                         { return subscriptions.TypeManualRebill }
func (h *ManualRebillHandler) Backoff(attempts int32) time.Duration { return h.Policy.Delay(attempts) }
func (h *ManualRebillHandler) now() time.Time                       { return h.Clock.Now().UTC() }
func (h *ManualRebillHandler) PrunePolicy() (bool, bool)            { return true, true }
func (h *ManualRebillHandler) CommitsTerminalOutcome() bool         { return true }

// Only this handler can release an accepted charge, under its domain lock and
// retained-custody check. A stale lease or changed catalog is not nonexecution.
func (h *ManualRebillHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (Relevance, error) {
	return StillRelevant(), nil
}

func (h *ManualRebillHandler) railClient(ctx context.Context, in gen.OpenrailsRailIntent) (*nmi.NMIClient, error) {
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, in)
	if err != nil {
		return nil, err
	}
	if !ok || client == nil {
		return nil, fmt.Errorf("nmi rail is not armed for provider %q", in.Rail)
	}
	merchantID, pspID := client.AccountIdentity()
	if in.PspID == nil || merchantID != in.MerchantID || pspID != *in.PspID {
		return nil, errors.New("rebill client is armed for another provider account")
	}
	return client, nil
}

func (h *ManualRebillHandler) Execute(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
	ctx = pinIntentAddress(ctx, in)
	p, err := subscriptions.DecodeManualRebillPayload(in)
	if err != nil {
		return Parked(err.Error())
	}
	if outcome, done := h.completeFromEvidence(ctx, in, p); done {
		return outcome
	}
	if EvidenceString(in, rebillSubmittedAt) != "" {
		return h.Verify(ctx, in)
	}
	if h.Config == nil {
		return Parked("rebill execution mode is not configured")
	}
	if blocked, reason := GateExecution(h.Config, Origin(in.Origin)); blocked {
		return Parked(reason)
	}
	client, err := h.railClient(ctx, in)
	if err != nil {
		return Parked(err.Error())
	}
	if client.ReadOnly {
		return Parked("nmi client is read-only")
	}
	if _, err := h.validateAndFence(ctx, in, p, false); err != nil {
		if errors.Is(err, errRebillSuperseded) || errors.Is(err, charge.ErrInstrumentChanged) {
			return h.finalizeNotExecuted(ctx, in, p, err.Error())
		}
		return Parked("validate accepted rebill: " + err.Error())
	}
	if err := h.prepareProvider(ctx, in, p, client); err != nil {
		return Parked("prepare accepted rebill: " + err.Error())
	}
	first, err := h.validateAndFence(ctx, in, p, true)
	if err != nil {
		if errors.Is(err, errRebillSuperseded) || errors.Is(err, charge.ErrInstrumentChanged) {
			return h.finalizeNotExecuted(ctx, in, p, err.Error())
		}
		return Parked("record rebill submission fence: " + err.Error())
	}
	if !first {
		return h.Verify(ctx, in)
	}
	posture := charge.RecurringMIT(p.Instrument.StoredCredentialRecurringRef)
	if p.Initiator == charge.InitiatorCustomer {
		posture = charge.RecurringReuse(p.Instrument.StoredCredentialRecurringRef)
	}
	response, err := client.AttemptManualRebill(ctx, nmi.ManualRebillParams{
		VaultID: p.Instrument.RailCustomerRef, BillingID: p.Instrument.RailMethodRef,
		SubscriptionID: p.RailSubscriptionID, OrderID: p.OrderReference, PONumber: p.OrderReference,
		StoredCredential: nmidirect.StoredCredentialFor(posture),
	})
	if errors.Is(err, nmi.ErrDuplicateTransaction) {
		// The matching recent charge on this card and amount may be NMI's own
		// schedule paying this period. Stay unknown: no new order is sent until
		// a provider read or an operator settles it.
		return Ambiguous("NMI refused the recovery as a duplicate of a recent charge on this card and amount; verify whether that charge paid this period")
	}
	if err != nil {
		return Ambiguous("rebill submission requires verification: " + err.Error())
	}
	if response == nil {
		return Ambiguous("rebill response is absent")
	}
	if !response.Success {
		if !response.Declined || response.ResponseCode <= 0 || nmi.UncertainResponseCode(response.ResponseCode) {
			return Ambiguous("rebill response requires provider verification")
		}
		if err := h.retainDecline(ctx, in, response.ResponseCode, response.TransactionID); err != nil {
			return Ambiguous("retain rebill refusal: " + err.Error())
		}
		current, err := NewStore(h.DB).Get(ctx, in.ID)
		if err != nil {
			return Ambiguous(err.Error())
		}
		return h.finalizeDecline(ctx, current, p)
	}
	if strings.TrimSpace(response.TransactionID) == "" {
		return Ambiguous("rebill approval has no transaction reference")
	}
	if err := NewStore(h.DB).RetainCollectionCandidate(ctx, in, CollectionCandidate{TransactionID: response.TransactionID}); err != nil {
		return Ambiguous("retain rebill candidate: " + err.Error())
	}
	return h.qualifyRebill(ctx, in, p, response.TransactionID)
}

// validateAndFence holds only the local subscription/method transaction. All
// provider traffic occurs outside it and uses the accepted references verbatim.
func (h *ManualRebillHandler) validateAndFence(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.ManualRebillPayload, fence bool) (bool, error) {
	first := false
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.Renewal.SubscriptionID)
		if err != nil {
			return err
		}
		failures := 0
		if sub.RetryAttempts != nil {
			failures = *sub.RetryAttempts
		}
		if sub.CollectionPolicy != models.CollectionPolicyProviderDunning || sub.Status != models.StatusPastDue || sub.CustomerID != p.Renewal.CustomerID || sub.PspID != p.Instrument.PSPID || string(sub.Rail) != p.Rail || sub.RailSubscriptionID != p.RailSubscriptionID || sub.PaymentMethodID == nil || *sub.PaymentMethodID != p.PaymentMethodID || sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.Equal(p.Renewal.PeriodStart) || failures != p.FailureCount {
			return errRebillSuperseded
		}
		paid, err := rebillPaymentAlreadyObserved(ctx, d, in.MerchantID, p)
		if err != nil {
			return err
		}
		if paid {
			return errRebillSuperseded
		}
		terms, err := subscriptions.PrepareRenewalTerms(ctx, d, sub, h.now(), nil)
		if err != nil {
			return err
		}
		terms.ProductName = p.Renewal.ProductName // display text cannot retarget an accepted charge
		if !reflect.DeepEqual(terms, p.Renewal) {
			return errRebillSuperseded
		}
		method, err := d.Gen(ctx).GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: in.MerchantID, ID: p.PaymentMethodID})
		if err != nil {
			return err
		}
		if err := p.Instrument.Matches(method, charge.AgreementRecurring); err != nil {
			return err
		}
		if method.CustomerID != p.Renewal.CustomerID {
			return errRebillSuperseded
		}
		if fence {
			first, err = NewStore(d).RecordProgressIfAbsent(ctx, in.ID, rebillSubmittedAt, h.now().Format(time.RFC3339Nano))
			return err
		}
		return nil
	})
	return first, err
}

func (h *ManualRebillHandler) Verify(ctx context.Context, in gen.OpenrailsRailIntent) Outcome {
	ctx = pinIntentAddress(ctx, in)
	current, err := NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return Ambiguous(err.Error())
	}
	p, err := subscriptions.DecodeManualRebillPayload(current)
	if err != nil {
		return Ambiguous(err.Error())
	}
	if outcome, done := h.completeFromEvidence(ctx, current, p); done {
		return outcome
	}
	if EvidenceString(current, rebillSubmittedAt) == "" {
		return h.Execute(ctx, current)
	}
	reference := ""
	if candidate, found, err := LoadCollectionCandidate(current); err != nil {
		return Ambiguous(err.Error())
	} else if found {
		reference = candidate.TransactionID
	}
	return h.qualifyRebill(ctx, current, p, reference)
}

func (h *ManualRebillHandler) qualifyRebill(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.ManualRebillPayload, reference string) Outcome {
	receipt, found, err := ReadNMICollectionReceipt(ctx, in, h.Resolver, reference)
	if err != nil {
		return Ambiguous("rebill receipt did not qualify: " + err.Error())
	}
	if !found {
		return Ambiguous("submitted rebill has no exact receipt; no automatic resend")
	}
	return h.finalizeSuccess(ctx, in, p, receipt)
}

func (h *ManualRebillHandler) completeFromEvidence(ctx context.Context, in gen.OpenrailsRailIntent, p subscriptions.ManualRebillPayload) (Outcome, bool) {
	receipt, found, err := LoadCollectedReceipt(in)
	if err != nil {
		return Ambiguous("retained rebill receipt is invalid: " + err.Error()), true
	}
	if found {
		return h.finalizeSuccess(ctx, in, p, receipt), true
	}
	if _, found, err := loadRebillDecline(in); err != nil {
		return Ambiguous(err.Error()), true
	} else if found {
		return h.finalizeDecline(ctx, in, p), true
	}
	return Outcome{}, false
}
