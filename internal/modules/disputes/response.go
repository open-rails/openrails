package disputes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/disputejob"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/reconcile"
)

// FindingResponseWaiting is a merchant's dispute responses held by a gate:
// raised once with its alert, resolved when none waits.
const FindingResponseWaiting reconcile.FindingType = "life.disputes.response_waiting"

// WaitError is a response a gate holds. The response is left waiting; a
// change of what holds it wakes it.
type WaitError struct{ Reason string }

func (e *WaitError) Error() string { return "dispute response waits: " + e.Reason }

// gates are what a step of a response waits for. Every step waits for a full
// write posture and the merchant's automation allowance; a case a pull found
// for the merchant to be armed; a destructive step for the switch.
type gates struct {
	destructive bool
	pulled      bool
}

// Respond carries out what a chargeback or fraud report calls for. A held
// step raises the merchant's waiting finding and ends the job.
func (s *Service) Respond(ctx context.Context, args disputejob.ResponseArgs) error {
	var err error
	switch args.Subject {
	case disputejob.SubjectDispute:
		err = s.respondChargeback(ctx, args.ID)
	case disputejob.SubjectFraudReport:
		err = s.respondFraudReport(ctx, args.ID)
	default:
		return fmt.Errorf("dispute response: unknown subject %q", args.Subject)
	}
	var wait *WaitError
	if errors.As(err, &wait) {
		return s.raiseWaiting(ctx, wait.Reason)
	}
	if err != nil {
		return err
	}
	return s.resolveWaiting(ctx)
}

// respondFraudReport refunds a fraud-reported charge in full, ends what it
// paid for and blocks its card: protective, so it does not wait for the
// destructive switch. Canceling the customer's other provider-billed
// subscriptions on the card at period end is destructive and does.
func (s *Service) respondFraudReport(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	report, err := s.DB.Gen(ctx).GetFraudReport(ctx, gen.GetFraudReportParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return fmt.Errorf("load fraud report: %w", err)
	}
	if !waitingReport(report) {
		return nil
	}
	payment, err := payments.NewPaymentService(s.DB, s.Clock).GetByID(ctx, report.PaymentID)
	if err != nil {
		return fmt.Errorf("load the reported charge: %w", err)
	}
	cause := fmt.Sprintf("FRAUD REPORT %s: %s", report.ProviderRef, report.FraudType)
	if report.EndedAt == nil {
		if report.Action == fraudPending {
			refunds, err := merchantconfig.FraudWarningRefunds(ctx, s.DB)
			if err != nil {
				return err
			}
			if !refunds {
				_, err := s.DB.Gen(ctx).DecideFraudReport(ctx, gen.DecideFraudReportParams{MerchantID: mid.UUID(), ID: id, Action: fraudRecorded, Now: s.now()})
				return err
			}
		}
		step := s.command(mid.UUID(), destructive.Refund, billing.PaymentID(payment.ID).String(), "fraud_report:"+id.String())
		err := s.admitted(ctx, step, gates{}, func(ctx context.Context, d *db.DB) error {
			q := d.Gen(ctx)
			locked, err := q.GetFraudReportForUpdate(ctx, gen.GetFraudReportForUpdateParams{MerchantID: mid.UUID(), PspID: report.PspID, ProviderRef: report.ProviderRef})
			if err != nil {
				return fmt.Errorf("lock fraud report: %w", err)
			}
			if !locked.Actionable || !waitingReport(locked) || locked.EndedAt != nil {
				return nil
			}
			if locked.Action == fraudPending {
				disputed, err := q.PaymentDisputeDecides(ctx, gen.PaymentDisputeDecidesParams{MerchantID: mid.UUID(), PaymentID: locked.PaymentID})
				if err != nil {
					return fmt.Errorf("check the charge's disputes: %w", err)
				}
				decision := gen.DecideFraudReportParams{MerchantID: mid.UUID(), ID: id, Action: fraudDisputed, Now: s.now()}
				if !disputed {
					reservation, err := intents.QueueFraudWarningRefund(ctx, d, payment, s.Clock)
					if err != nil {
						return err
					}
					decision.Action = fraudNothingToRefund
					if reservation != nil {
						decision.Action, decision.RefundPaymentID = fraudRefunded, reservation
					}
				}
				if _, err := q.DecideFraudReport(ctx, decision); err != nil {
					return fmt.Errorf("decide fraud report: %w", err)
				}
				if disputed {
					return nil
				}
			}
			own, err := ChargeSubscriptions(ctx, d, payment)
			if err != nil {
				return err
			}
			for _, sub := range own {
				if err := endSubscription(ctx, d, s.Lifecycle, sub, models.CancelTypeMerchant, true, cause); err != nil {
					return err
				}
			}
			if payment.SubscriptionID == nil {
				if err := entitlements.NewEntitlementService(d, s.Clock).EndActiveByPayment(ctx, payment.ID, models.AccessRevokeFraud); err != nil {
					return fmt.Errorf("end the charge's access: %w", err)
				}
			}
			if err := s.BlockCard(ctx, d, payment, nil); err != nil {
				return err
			}
			if err := q.MarkFraudReportEnded(ctx, gen.MarkFraudReportEndedParams{MerchantID: mid.UUID(), ID: id, EndedAt: s.now()}); err != nil {
				return err
			}
			return s.respondedIfAlone(ctx, d, payment, func() error {
				return q.MarkFraudReportResponded(ctx, gen.MarkFraudReportRespondedParams{MerchantID: mid.UUID(), ID: id, RespondedAt: s.now()})
			})
		})
		if err != nil {
			return err
		}
		if report, err = s.DB.Gen(ctx).GetFraudReport(ctx, gen.GetFraudReportParams{MerchantID: mid.UUID(), ID: id}); err != nil || !waitingReport(report) {
			return err
		}
	}
	step := s.command(mid.UUID(), destructive.CancelSubscription, billing.PaymentID(payment.ID).String(), "fraud_report:"+id.String()+":cancel")
	return s.admitted(ctx, step, gates{destructive: true}, func(ctx context.Context, d *db.DB) error {
		return s.cancelOthers(ctx, d, payment, cause, func() error {
			return d.Gen(ctx).MarkFraudReportResponded(ctx, gen.MarkFraudReportRespondedParams{MerchantID: mid.UUID(), ID: id, RespondedAt: s.now()})
		})
	})
}

// respondChargeback blocks the charged-back card, protective, and cancels at
// period end the customer's other subscriptions a provider bills on it,
// destructive: OpenRails cannot keep a provider from charging it again.
func (s *Service) respondChargeback(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.DB.Gen(ctx)
	dispute, err := q.GetDispute(ctx, gen.GetDisputeParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return fmt.Errorf("load dispute: %w", err)
	}
	if dispute.RespondedAt != nil {
		return nil
	}
	done := func(ctx context.Context, d *db.DB) error {
		return d.Gen(ctx).MarkDisputeResponded(ctx, gen.MarkDisputeRespondedParams{MerchantID: mid.UUID(), ID: id, RespondedAt: s.now()})
	}
	if dispute.Kind != string(billing.DisputeChargeback) || dispute.Status == string(billing.DisputeWon) || dispute.PaymentID == nil {
		return done(ctx, s.DB)
	}
	payment, err := payments.NewPaymentService(s.DB, s.Clock).GetByID(ctx, *dispute.PaymentID)
	if err != nil {
		return fmt.Errorf("load the disputed charge: %w", err)
	}
	g := gates{pulled: dispute.ObservedVia == "pull"}
	block := s.command(mid.UUID(), destructive.BlockPaymentMethod, billing.DisputeID(id).String(), "dispute:"+id.String()+":block")
	if err := s.admitted(ctx, block, g, func(ctx context.Context, d *db.DB) error {
		if err := s.BlockCard(ctx, d, payment, &dispute); err != nil {
			return err
		}
		return s.respondedIfAlone(ctx, d, payment, func() error { return done(ctx, d) })
	}); err != nil {
		return err
	}
	if dispute, err = q.GetDispute(ctx, gen.GetDisputeParams{MerchantID: mid.UUID(), ID: id}); err != nil || dispute.RespondedAt != nil {
		return err
	}
	g.destructive = true
	cause := fmt.Sprintf("CHARGEBACK %s on the card", billing.DisputeID(id))
	cancel := s.command(mid.UUID(), destructive.CancelSubscription, billing.DisputeID(id).String(), "dispute:"+id.String()+":cancel")
	return s.admitted(ctx, cancel, g, func(ctx context.Context, d *db.DB) error {
		return s.cancelOthers(ctx, d, payment, cause, func() error { return done(ctx, d) })
	})
}

// respondedIfAlone finishes the response when no other subscription is
// billed on the card: nothing destructive remains.
func (s *Service) respondedIfAlone(ctx context.Context, d *db.DB, payment *models.Payment, finish func() error) error {
	others, err := providerBilledOnCard(ctx, d, payment)
	if err != nil || len(others) > 0 {
		return err
	}
	return finish()
}

// cancelOthers cancels at period end the customer's other subscriptions a
// provider bills on the charge's card, then finishes the response.
func (s *Service) cancelOthers(ctx context.Context, d *db.DB, payment *models.Payment, cause string, finish func() error) error {
	others, err := providerBilledOnCard(ctx, d, payment)
	if err != nil {
		return err
	}
	for _, sub := range others {
		if err := endSubscription(ctx, d, s.Lifecycle, sub, models.CancelTypeChargeback, false, cause); err != nil {
			return err
		}
	}
	return finish()
}

// command is one step of a response: keyed and digested on its subject and
// step alone, so a retry after the world changed admits the same command.
func (s *Service) command(mid uuid.UUID, kind destructive.Kind, target, key string) destructive.Command {
	return destructive.Command{MerchantID: mid, Kind: kind, Target: target, Key: key, Digest: destructive.Digest([]string{key}), Units: 1}
}

// admitted runs effect in one transaction with the command's outcome once
// its gates admit it; otherwise it answers a WaitError. An effect that rolled
// back changed nothing and releases its unit.
func (s *Service) admitted(ctx context.Context, command destructive.Command, g gates, effect func(context.Context, *db.DB) error) error {
	if s.Admissions == nil || s.Postures == nil || s.Lifecycle == nil {
		return &WaitError{Reason: "responses are not wired"}
	}
	policy, err := s.DB.Gen(ctx).GetDestructivePolicy(ctx, command.MerchantID)
	if err != nil {
		return fmt.Errorf("read the destructive policy: %w", err)
	}
	// Only the deployment-wide switch is a protective step's to pass; the
	// merchant's own emergency stop holds every step.
	switch {
	case !policy.MerchantEnabled:
		return &WaitError{Reason: "the merchant's destructive actions are stopped"}
	case g.pulled && policy.EnforceArmedAt == nil:
		return &WaitError{Reason: "a case a pull found waits for the merchant to be armed for enforcing pulls"}
	case g.destructive && !policy.SwitchEnabled:
		return &WaitError{Reason: "the destructive switch is off: canceling the customer's other subscriptions on the card waits for it"}
	}
	posture, err := s.Postures.Read(ctx, command.MerchantID)
	if err != nil {
		return err
	}
	if posture.Limited() {
		return &WaitError{Reason: posture.Reason}
	}
	action, err := s.Admissions.AdmitAutonomous(ctx, command, destructive.DisputeSource)
	var held *destructive.HeldError
	if errors.As(err, &held) {
		return &WaitError{Reason: "held by destructive hold " + held.Hold.ID.String()}
	}
	if err != nil {
		return fmt.Errorf("admit dispute response: %w", err)
	}
	if action.State != destructive.StateSucceeded {
		if action, err = s.Admissions.Start(ctx, command.MerchantID, action.ID); err != nil {
			if errors.As(err, &held) {
				return &WaitError{Reason: "held by destructive hold " + held.Hold.ID.String()}
			}
			return fmt.Errorf("start dispute response: %w", err)
		}
	}
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := effect(ctx, s.DB.NewWithPgxTx(tx)); err != nil {
			return err
		}
		if action.State == destructive.StateSucceeded {
			return nil
		}
		return s.Admissions.FinishTx(ctx, tx, command.MerchantID, action.ID, destructive.OutcomeSucceeded, command.Target)
	})
	if err == nil || action.State == destructive.StateSucceeded {
		return err
	}
	finish, cancel := db.DetachedWriteContext(ctx, 10*time.Second)
	defer cancel()
	return errors.Join(err, s.Admissions.Finish(finish, command.MerchantID, action.ID, destructive.OutcomeNoEffect, ""))
}

// raiseWaiting records that the merchant's responses wait, alerting once.
func (s *Service) raiseWaiting(ctx context.Context, reason string) error {
	rec, err := (&reconcile.PGStore{DB: s.DB}).RaiseFinding(ctx, reconcile.RaisedFinding{Type: FindingResponseWaiting, SubjectKey: "responses",
		Severity: reconcile.SeverityHigh, RecommendedAction: "Dispute responses wait: " + reason + ". They run when it changes.",
		Evidence: map[string]any{"reason": reason}})
	if err != nil {
		return fmt.Errorf("raise waiting dispute responses: %w", err)
	}
	return s.notify(ctx, rec)
}

// resolveWaiting resolves the waiting finding once no response waits.
func (s *Service) resolveWaiting(ctx context.Context) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	waiting, err := s.DB.Gen(ctx).MerchantHasWaitingDisputeResponses(ctx, mid.UUID())
	if err != nil || waiting {
		return err
	}
	return (&reconcile.PGStore{DB: s.DB}).ResolveRaisedFinding(ctx, FindingResponseWaiting, "responses")
}
