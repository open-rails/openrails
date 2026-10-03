package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/failpoint"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/attempts"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const rebillDeclineKey = "rebill_decline"

type rebillDecline struct {
	Binding           receiptBinding `json:"binding"`
	ResponseCode      int            `json:"response_code"`
	ProviderReference string         `json:"provider_reference"`
}

// ValidateManualRebillTerminal is the archive/reload boundary for this exact
// kind. Unsupported or contradictory evidence refuses export and restore;
// callers cannot silently drop a retained field to make the record portable.
func ValidateManualRebillTerminal(in gen.BillingRailIntent) error {
	if _, err := subscriptions.DecodeManualRebillPayload(in); err != nil {
		return err
	}
	_, paid, err := LoadCollectedReceipt(in)
	if err != nil {
		return err
	}
	if _, _, err := loadRebillPreparation(in); err != nil {
		return err
	}
	refusal, declined, err := loadRebillDecline(in)
	if err != nil {
		return err
	}
	if in.Status == StatusSucceeded {
		if !paid {
			return errors.New("succeeded rebill has no qualified receipt")
		}
		return nil
	}
	if paid {
		return errors.New("collected rebill has an incompatible terminal state")
	}
	if in.Status != StatusFailedTerminal && in.Status != StatusExpired && in.Status != StatusSuperseded {
		return errors.New("rebill archive is not terminal")
	}
	if in.Status == StatusFailedTerminal && declined {
		var projection struct {
			Declined     bool `json:"declined"`
			ResponseCode int  `json:"response_code"`
		}
		if err := json.Unmarshal(in.ResultEvidence, &projection); err != nil {
			return err
		}
		if !projection.Declined || projection.ResponseCode != refusal.ResponseCode {
			return errors.New("rebill refusal projection contradicts retained facts")
		}
	}
	if in.Status == StatusFailedTerminal && !declined {
		var evidence struct {
			NotExecuted bool `json:"not_executed"`
		}
		if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
			return err
		}
		if !evidence.NotExecuted || EvidenceString(in, rebillSubmittedAt) != "" {
			return errors.New("failed rebill has no qualified refusal or pre-submission release")
		}
	}
	if (in.Status == StatusExpired || in.Status == StatusSuperseded) && EvidenceString(in, rebillSubmittedAt) != "" {
		return errors.New("submitted rebill cannot be an expired or superseded archive")
	}
	return nil
}

func loadRebillDecline(in gen.BillingRailIntent) (rebillDecline, bool, error) {
	var all map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return rebillDecline{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &all); err != nil {
		return rebillDecline{}, false, err
	}
	raw, found := all[rebillDeclineKey]
	if !found {
		return rebillDecline{}, false, nil
	}
	var fact rebillDecline
	if err := json.Unmarshal(raw, &fact); err != nil {
		return fact, true, err
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return fact, true, err
	}
	if (in.IntentType != subscriptions.TypeManualRebill && in.IntentType != subscriptions.TypeSubscriptionCollection) || fact.Binding != binding || (fact.ResponseCode <= 0 || nmi.UncertainResponseCode(fact.ResponseCode)) {
		return fact, true, errors.New("rebill refusal is not bound to accepted terms")
	}
	return fact, true, nil
}
func (h *ManualRebillHandler) retainDecline(ctx context.Context, in gen.BillingRailIntent, code int, reference string) error {
	return NewStore(h.DB).RetainRecurringDecline(ctx, in, code, reference)
}

// LoadRecurringDecline exposes only bound, positive refusal facts.
func LoadRecurringDecline(in gen.BillingRailIntent) (int, string, bool, error) {
	fact, found, err := loadRebillDecline(in)
	return fact.ResponseCode, fact.ProviderReference, found, err
}

func (s *Store) RetainRecurringDecline(ctx context.Context, in gen.BillingRailIntent, code int, reference string) error {
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	if code <= 0 || nmi.UncertainResponseCode(code) {
		return errors.New("ambiguous response cannot be retained as a decline")
	}
	raw, err := json.Marshal(rebillDecline{binding, code, reference})
	if err != nil {
		return err
	}
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	n, err := s.db.Gen(ctx).RetainRailIntentRebillDecline(ctx, gen.RetainRailIntentRebillDeclineParams{ID: in.ID, MerchantID: in.MerchantID, PspID: *in.PspID, Payload: in.Payload, Decline: raw})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("rebill refusal custody rejected a changed or terminal operation")
	}
	return nil
}
func (h *ManualRebillHandler) lifecycle(d *db.DB) *subscriptions.SubscriptionLifecycleService {
	lifecycle := subscriptions.NewSubscriptionLifecycleService(d, catalog.NewProductService(d), catalog.NewPriceService(d), entitlements.NewEntitlementService(d, h.Clock), nil, payments.NewPaymentService(d, h.Clock), h.Clock)
	lifecycle.SetConfig(h.Config)
	lifecycle.SetProviderCancelScheduler(h.DeferDelete)
	return lifecycle
}
func (h *ManualRebillHandler) finalizeSuccess(ctx context.Context, in gen.BillingRailIntent, p subscriptions.ManualRebillPayload, receipt CollectedReceipt) Outcome {
	ctx = pinIntentAddress(ctx, in)
	retained, err := NewStore(h.DB).RetainCollectedReceipt(ctx, in, receipt)
	if err != nil {
		return Ambiguous("retain rebill receipt before local completion: " + err.Error())
	}
	outcome := Succeeded(map[string]any{"transaction_id": retained.TransactionID(), "rail": p.Rail, "verified_existing": true})
	if err := hitFailpoint(ctx, in, failpoint.BeforeComplete); err != nil {
		return Ambiguous(err.Error())
	}
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.Renewal.SubscriptionID)
		if err != nil {
			return err
		}
		current, err := NewStore(d).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		if current.Status == StatusSucceeded {
			return NewStore(d).CompleteManualRebill(ctx, in, outcome, h.now())
		}
		if err := recordRebillAttempt(ctx, d, in, p, sub, attempts.Attempt{Approved: true, TransactionID: retained.TransactionID()}, h.now()); err != nil {
			return err
		}
		lifecycle := h.lifecycle(d)
		params := &subscriptions.RenewMembershipParams{Prepared: &p.Renewal, Rail: models.Rail(p.Rail), RailSubscriptionID: p.RailSubscriptionID, TransactionID: retained.TransactionID(), Amount: p.Renewal.Amount, AmountProvided: true, Currency: p.Renewal.Currency}
		if _, terminal := subscriptions.TerminalCancelReason(sub); terminal || (sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(p.Renewal.PeriodEnd)) {
			err = lifecycle.RecordConfirmedChargeWithoutRenewal(ctx, params)
		} else {
			err = lifecycle.RenewMembership(ctx, params)
		}
		if err != nil {
			return err
		}
		return NewStore(d).CompleteManualRebill(ctx, in, outcome, h.now())
	})
	if err != nil {
		return Ambiguous("rebill paid; local completion will resume from retained receipt: " + err.Error())
	}
	return outcome
}
func (h *ManualRebillHandler) finalizeDecline(ctx context.Context, in gen.BillingRailIntent, p subscriptions.ManualRebillPayload) Outcome {
	ctx = pinIntentAddress(ctx, in)
	refusal, found, err := loadRebillDecline(in)
	if err != nil || !found {
		return Ambiguous("rebill has no valid retained refusal")
	}
	if err := hitFailpoint(ctx, in, failpoint.BeforeComplete); err != nil {
		return Ambiguous(err.Error())
	}
	code := fmt.Sprint(refusal.ResponseCode)
	outcome := TerminalWithEvidence("rebill declined", map[string]any{"declined": true, "response_code": refusal.ResponseCode})
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		sub, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.Renewal.SubscriptionID)
		if err != nil {
			return err
		}
		current, err := NewStore(d).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		if current.Status == StatusFailedTerminal {
			return NewStore(d).CompleteManualRebill(ctx, in, outcome, h.now())
		}
		if err := recordRebillAttempt(ctx, d, in, p, sub, attempts.Attempt{Answer: decline.Evidence{Code: code}, TransactionID: refusal.ProviderReference}, h.now()); err != nil {
			return err
		}
		failures := 0
		if sub.RetryAttempts != nil {
			failures = *sub.RetryAttempts
		}
		// A stale refusal is still forensic evidence, but cannot dunn a later
		// period or overwrite a recovery observed while this attempt ran.
		if (sub.Status == models.StatusPastDue || sub.Status == models.StatusAwaitingMethod) && sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.Equal(p.Renewal.PeriodStart) && failures == p.FailureCount {
			action := decline.Classify(string(models.RailNMI), code).Action
			certainty := ""
			if action == decline.NonRecoverable {
				certainty = collection.CertaintyNonRetryableDecline
			}
			blocked := ""
			if verdict := destructive.New(d).Check(ctx, in.MerchantID); !verdict.Allowed {
				blocked = verdict.Reason
			}
			reason := "rebill declined"
			err = h.lifecycle(d).FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: models.Rail(p.Rail), SubscriptionID: &p.Renewal.SubscriptionID, FailureCode: &code, FailureReason: &reason, Decline: action, AttemptRecorded: true, TerminalCertainty: certainty, TerminalBlocked: blocked})
			if err != nil {
				return err
			}
		}
		return NewStore(d).CompleteManualRebill(ctx, in, outcome, h.now())
	})
	if err != nil {
		return Ambiguous("rebill refused; local completion will resume: " + err.Error())
	}
	return outcome
}
func (h *ManualRebillHandler) finalizeNotExecuted(ctx context.Context, in gen.BillingRailIntent, p subscriptions.ManualRebillPayload, reason string) Outcome {
	ctx = pinIntentAddress(ctx, in)
	outcome := TerminalWithEvidence(reason, map[string]any{"declined": false, "not_executed": true})
	ctx, cancel := LedgerWriteContext(ctx)
	defer cancel()
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		if _, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, p.Renewal.SubscriptionID); err != nil {
			return err
		}
		return NewStore(d).CompleteManualRebill(ctx, in, outcome, h.now())
	})
	if err != nil {
		return Ambiguous("cannot release accepted rebill: " + err.Error())
	}
	return outcome
}

// recordRebillAttempt records an OpenRails rebill's answer for its cycle
// (#1111) in the completion transaction.
func recordRebillAttempt(ctx context.Context, d *db.DB, in gen.BillingRailIntent, p subscriptions.ManualRebillPayload, sub *models.Subscription, a attempts.Attempt, at time.Time) error {
	a.MerchantID, a.CustomerID, a.PSPID, a.Rail = in.MerchantID, p.Renewal.CustomerID, p.Instrument.PSPID, p.Rail
	a.Kind, a.Owner, a.At, a.Step = attempts.RebillKind(p.Initiator == charge.InitiatorCustomer, p.FailureCount), attempts.OwnerOf(sub.CollectionPolicy), at, "charge"
	a.Amount, a.Currency, a.PaymentMethodID, a.RailIntentID = p.Renewal.Amount, p.Renewal.Currency, &p.PaymentMethodID, &in.ID
	a.Cycle = &attempts.Cycle{SubscriptionID: p.Renewal.SubscriptionID, DueAt: p.Renewal.PeriodStart}
	a.TokenType = payments.DefaultTokenType(p.Rail, p.Instrument.Custodian)
	return attempts.Record(ctx, d.Gen(ctx), a)
}
