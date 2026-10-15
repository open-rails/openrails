package intents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/lifecycle"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/writeposture"
)

const confirmationSubmitted = "confirmation_submitted_at"
const confirmationAnswer = "confirmation_answer"

var errConfirmationChanged = errors.New("same-card confirmation is no longer applicable")

type NMIRecurringConfirmationHandler struct {
	DB       *db.DB
	Config   *config.Config
	Resolver NMIClientResolver
	Clock    clockwork.Clock
}

func NewNMIRecurringConfirmationHandler(d *db.DB, cfg *config.Config, resolver NMIClientResolver, clock clockwork.Clock) *NMIRecurringConfirmationHandler {
	return &NMIRecurringConfirmationHandler{d, cfg, resolver, timeutil.FirstClock(clock)}
}
func (h *NMIRecurringConfirmationHandler) Type() string { return TypeNMIRecurringConfirmation }
func (h *NMIRecurringConfirmationHandler) Backoff(n int32) time.Duration {
	return DefaultBackoff.Delay(n)
}
func (h *NMIRecurringConfirmationHandler) PrunePolicy() (bool, bool)    { return true, true }
func (h *NMIRecurringConfirmationHandler) CommitsTerminalOutcome() bool { return true }
func (h *NMIRecurringConfirmationHandler) CheckRelevance(context.Context, gen.BillingProviderIntent) (Relevance, error) {
	// A submitted verification still needs its answer after a local card change.
	return StillRelevant(), nil
}

func decodeConfirmation(in gen.BillingProviderIntent) (recurringConfirmation, error) {
	var p recurringConfirmation
	if err := json.Unmarshal(in.Payload, &p); err != nil {
		return p, err
	}
	if in.IntentType != TypeNMIRecurringConfirmation || in.SubscriptionID == nil || *in.SubscriptionID != p.SubscriptionID || in.PspID == nil || *in.PspID != p.Instrument.PSPID || in.Rail != p.Rail || p.OrderReference == "" || p.AcceptedAt.IsZero() || p.PeriodEnd.IsZero() {
		return p, errors.New("invalid recurring confirmation binding")
	}
	return p, nil
}

func (h *NMIRecurringConfirmationHandler) client(ctx context.Context, in gen.BillingProviderIntent) (*nmi.NMIClient, error) {
	client, ok, err := resolveIntentNMIClient(ctx, h.Resolver, in)
	if err != nil {
		return nil, err
	}
	if !ok || client == nil {
		return nil, errors.New("NMI account is unavailable")
	}
	mid, psp := client.AccountIdentity()
	if mid != in.MerchantID || in.PspID == nil || psp != *in.PspID {
		return nil, errors.New("confirmation account does not match operation")
	}
	return client, nil
}

func (h *NMIRecurringConfirmationHandler) Execute(ctx context.Context, in gen.BillingProviderIntent) Outcome {
	ctx = pinIntentAddress(ctx, in)
	p, err := decodeConfirmation(in)
	if err != nil {
		return Parked(err.Error())
	}
	if EvidenceString(in, confirmationSubmitted) != "" {
		return h.Verify(ctx, in)
	}
	if h.Config == nil {
		return Parked("confirmation execution mode is not configured")
	}
	if blocked, reason := GateExecution(ctx, writeposture.View{Config: h.Config, DB: h.DB}, in.MerchantID, Origin(in.Origin)); blocked {
		return Parked(reason)
	}
	client, err := h.client(ctx, in)
	if err != nil {
		return Parked(err.Error())
	}
	if client.ReadOnly {
		return Parked("NMI client is read-only")
	}
	first := false
	var refused *Outcome
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.SubscriptionID)
		if err != nil {
			return err
		}
		stored, err := NewStore(d).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		// A previous executor may have fenced after this runner read the row.
		if EvidenceString(stored, confirmationSubmitted) != "" {
			return nil
		}
		if err := NewStore(h.DB).RequireClaim(ctx, in.ID, h.Clock.Now()); err != nil {
			return err
		}
		current, eligible, err := confirmationSnapshot(ctx, d, sub)
		if err != nil {
			return err
		}
		if !eligible || !reflect.DeepEqual(current, p.confirmationTerms) {
			out := TerminalWithEvidence(errConfirmationChanged.Error(), map[string]any{"not_executed": true})
			refused = &out
			return NewStore(d).MarkFailedTerminal(ctx, in.ID, out.Reason, out.Evidence)
		}
		if err := subscriptions.RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
			return err
		}
		first, err = NewStore(d).RecordProgressIfAbsent(ctx, in.ID, confirmationSubmitted, h.Clock.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	if err != nil {
		return Parked(err.Error())
	}
	if refused != nil {
		return *refused
	}
	if !first {
		return h.Verify(ctx, in)
	}
	if err := NewStore(h.DB).RequireClaim(ctx, in.ID, h.Clock.Now()); err != nil {
		return Ambiguous(err.Error())
	}
	ref, err := client.VerifyStoredCredential(ctx, p.Instrument.RailCustomerRef, p.Instrument.RailMethodRef, p.OrderReference, true)
	answer := nmi.VerificationEvidence{TransactionID: ref, ResponseCode: 100}
	if err != nil {
		var refusal *nmi.CustomerVaultError
		if !errors.As(err, &refusal) || refusal.ResponseCode < 200 || refusal.ResponseCode >= 300 {
			return Ambiguous("confirmation sent; awaiting provider verification")
		}
		answer = nmi.VerificationEvidence{TransactionID: refusal.TransactionID, ResponseCode: refusal.ResponseCode}
	}
	return h.retainAndComplete(ctx, in, answer)
}

func (h *NMIRecurringConfirmationHandler) Verify(ctx context.Context, in gen.BillingProviderIntent) Outcome {
	ctx = pinIntentAddress(ctx, in)
	current, err := NewStore(h.DB).Get(ctx, in.ID)
	if err != nil {
		return Ambiguous(err.Error())
	}
	p, err := decodeConfirmation(current)
	if err != nil {
		return Ambiguous(err.Error())
	}
	var evidence struct {
		Answer *nmi.VerificationEvidence `json:"confirmation_answer"`
	}
	if err := json.Unmarshal(current.ResultEvidence, &evidence); err != nil && len(current.ResultEvidence) > 0 {
		return Ambiguous(err.Error())
	}
	if evidence.Answer != nil {
		return h.complete(ctx, current, p, *evidence.Answer)
	}
	if EvidenceString(current, confirmationSubmitted) == "" {
		return Retryable("confirmation awaits gated execution")
	}
	client, err := h.client(ctx, current)
	if err != nil {
		return Ambiguous(err.Error())
	}
	answer, err := client.ReadVerificationEvidence(ctx, p.OrderReference, p.Instrument.RailCustomerRef)
	if err != nil {
		return Ambiguous(err.Error())
	}
	if answer.ResponseCode == 0 {
		return Ambiguous("submitted confirmation has no conclusive result; no automatic resend")
	}
	return h.retainAndComplete(ctx, current, answer)
}

func (h *NMIRecurringConfirmationHandler) retainAndComplete(ctx context.Context, in gen.BillingProviderIntent, answer nmi.VerificationEvidence) Outcome {
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	if _, err := NewStore(h.DB).RecordProgressIfAbsent(ctx, in.ID, confirmationAnswer, answer); err != nil {
		return Ambiguous("retain confirmation result: " + err.Error())
	}
	// Reload the first retained answer, including when a previous executor won.
	return h.Verify(ctx, in)
}

func (h *NMIRecurringConfirmationHandler) complete(ctx context.Context, in gen.BillingProviderIntent, p recurringConfirmation, answer nmi.VerificationEvidence) Outcome {
	if err := hitFailpoint(ctx, in, failpoint.BeforeComplete); err != nil {
		return Ambiguous(err.Error())
	}
	approved := answer.ResponseCode == 100 && answer.TransactionID != ""
	if !approved && (answer.ResponseCode < 200 || answer.ResponseCode >= 300) {
		return Ambiguous("retained confirmation has no conclusive answer")
	}
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	out := Succeeded(map[string]any{"transaction_id": answer.TransactionID, "response_code": answer.ResponseCode, confirmationAnswer: answer, confirmationSubmitted: EvidenceString(in, confirmationSubmitted)})
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		repo := subscriptions.NewSubscriptionRepo(d)
		sub, err := repo.GetByIDForUpdate(ctx, p.SubscriptionID)
		if err != nil {
			return err
		}
		stored, err := NewStore(d).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		if stored.Status == StatusSucceeded {
			return nil
		}
		if stored.Status == StatusFailedTerminal {
			out = TerminalWithEvidence("confirmation refused", out.Evidence)
			return nil
		}
		now := h.Clock.Now().UTC()
		if err := attempts.Record(ctx, d.Gen(ctx), attempts.Attempt{
			MerchantID: in.MerchantID, CustomerID: p.CustomerID, PSPID: p.Instrument.PSPID, Rail: p.Rail,
			Kind: attempts.Verify, Owner: attempts.OwnerNMISchedule, Approved: approved,
			Answer: decline.Evidence{Code: strconv.Itoa(answer.ResponseCode)}, TransactionID: answer.TransactionID,
			SubscriptionID: &p.SubscriptionID, PaymentMethodID: &p.PaymentMethodID, ProviderIntentID: &in.ID,
			Step: "verify", At: now, TokenType: charge.TokenTypePSPToken,
		}); err != nil {
			return err
		}
		current, eligible, err := confirmationSnapshot(ctx, d, sub)
		if err != nil {
			return err
		}
		if !approved || !eligible || !reflect.DeepEqual(current, p.confirmationTerms) {
			reason := "card verification declined"
			if approved {
				reason = errConfirmationChanged.Error()
			}
			out = TerminalWithEvidence(reason, out.Evidence)
			return NewStore(d).MarkFailedTerminal(ctx, in.ID, reason, out.Evidence)
		}
		if err := subscriptions.RefuseOwnedRebillTerms(ctx, d, sub); err != nil {
			return err
		}
		_, err = mandates.Replace(ctx, d.Gen(ctx), mandates.Agreement{MerchantID: in.MerchantID, CustomerID: p.CustomerID, PaymentMethodID: p.PaymentMethodID, PSPID: p.Instrument.PSPID, Rail: p.Rail, Kind: charge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: &charge.Mandate{InitialTransactionID: answer.TransactionID}, AcceptedAt: p.AcceptedAt}, now)
		if err != nil {
			return err
		}
		if _, err := subscriptions.Transition(sub, lifecycle.RecurringAgreementRestored{}, now); err != nil {
			return err
		}
		sub.NextRetryAt = &now
		sub.PaymentMethodID = &p.PaymentMethodID
		sub.RecordHistoryDecision(lifecycle.Name(lifecycle.RecurringAgreementRestored{}))
		if err := repo.UpdateAt(ctx, sub, now); err != nil {
			return err
		}
		return NewStore(d).MarkSucceeded(ctx, in.ID, now, out.Evidence)
	})
	if err != nil {
		return Ambiguous("confirmation retained; local completion pending: " + err.Error())
	}
	return out
}
