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
)

// responseSource is the autonomous budget responses draw on.
const responseSource = "dispute_response"

// WaitError is a response the destructive gates hold: the job waits and
// tries again, so it is never dropped.
type WaitError struct{ Reason string }

func (e *WaitError) Error() string { return "dispute response waits: " + e.Reason }

// Respond carries out what a chargeback or fraud report calls for, all of it
// in one transaction once the kill switch, the merchant's write posture and
// the automation budget admit it.
func (s *Service) Respond(ctx context.Context, args disputejob.ResponseArgs) error {
	switch args.Subject {
	case disputejob.SubjectDispute:
		return s.respondChargeback(ctx, args.ID)
	case disputejob.SubjectFraudReport:
		return s.respondFraudReport(ctx, args.ID)
	}
	return fmt.Errorf("dispute response: unknown subject %q", args.Subject)
}

// respondFraudReport refunds a fraud-reported charge in full, ends what it
// paid for and blocks its card. A charge a dispute decides is left to it.
func (s *Service) respondFraudReport(ctx context.Context, id uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.DB.Gen(ctx)
	report, err := q.GetFraudReport(ctx, gen.GetFraudReportParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return fmt.Errorf("load fraud report: %w", err)
	}
	if !report.Actionable || report.EndedAt != nil || report.Action == fraudDisputed {
		return nil
	}
	if report.Action == fraudRecorded {
		refunds, err := merchantconfig.FraudWarningRefunds(ctx, s.DB)
		if err != nil || !refunds {
			return err
		}
		disputed, err := q.PaymentDisputeDecides(ctx, gen.PaymentDisputeDecidesParams{MerchantID: mid.UUID(), PaymentID: report.PaymentID})
		if err != nil {
			return fmt.Errorf("check the charge's disputes: %w", err)
		}
		if disputed {
			_, err := q.DecideFraudReport(ctx, gen.DecideFraudReportParams{MerchantID: mid.UUID(), ID: report.ID, Action: fraudDisputed})
			return err
		}
	}
	payment, err := payments.NewPaymentService(s.DB, s.Clock).GetByID(ctx, report.PaymentID)
	if err != nil {
		return fmt.Errorf("load the reported charge: %w", err)
	}
	own, err := ChargeSubscriptions(ctx, s.DB, payment)
	if err != nil {
		return err
	}
	others, err := providerBilledOnCard(ctx, s.DB, payment)
	if err != nil {
		return err
	}
	cause := fmt.Sprintf("FRAUD REPORT %s: %s", report.ProviderRef, report.FraudType)
	command := destructive.Command{MerchantID: mid.UUID(), Kind: destructive.Refund, Target: billing.PaymentID(payment.ID).String(),
		Key: "fraud_report:" + report.ID.String(), Digest: destructive.Digest([]string{report.ID.String(), payment.ID.String()}),
		Units: int64(1 + len(own) + len(others))}
	return s.admitted(ctx, command, func(ctx context.Context, d *db.DB) error {
		q := d.Gen(ctx)
		locked, err := q.GetFraudReportForUpdate(ctx, gen.GetFraudReportForUpdateParams{MerchantID: mid.UUID(), PspID: report.PspID, ProviderRef: report.ProviderRef})
		if err != nil {
			return fmt.Errorf("lock fraud report: %w", err)
		}
		if locked.EndedAt != nil {
			return nil
		}
		if locked.Action == fraudRecorded {
			reservation, err := intents.QueueFraudWarningRefund(ctx, d, payment, s.Clock)
			if err != nil {
				return err
			}
			decision := gen.DecideFraudReportParams{MerchantID: mid.UUID(), ID: locked.ID, Action: fraudNothingToRefund}
			if reservation != nil {
				decision.Action, decision.RefundPaymentID = fraudRefunded, reservation
			}
			if _, err := q.DecideFraudReport(ctx, decision); err != nil {
				return fmt.Errorf("decide fraud report: %w", err)
			}
		}
		if err := s.endCharge(ctx, d, payment, own, others, models.CancelTypeMerchant, models.AccessRevokeFraud, cause, nil); err != nil {
			return err
		}
		return q.MarkFraudReportEnded(ctx, gen.MarkFraudReportEndedParams{MerchantID: mid.UUID(), ID: locked.ID, EndedAt: s.now()})
	})
}

// respondChargeback blocks the charged-back card and cancels, at period end,
// the customer's other subscriptions a provider bills on it: OpenRails cannot
// keep a provider from charging it again.
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
	// Blocking the card only protects it; a pulled chargeback has had none.
	if err := s.BlockCard(ctx, s.DB, payment, &dispute); err != nil {
		return err
	}
	others, err := providerBilledOnCard(ctx, s.DB, payment)
	if err != nil {
		return err
	}
	if len(others) == 0 {
		return done(ctx, s.DB)
	}
	cause := fmt.Sprintf("CHARGEBACK %s on the card", billing.DisputeID(id))
	command := destructive.Command{MerchantID: mid.UUID(), Kind: destructive.CancelSubscription, Target: billing.DisputeID(id).String(),
		Key: "dispute:" + id.String(), Digest: destructive.Digest(others), Units: int64(len(others))}
	return s.admitted(ctx, command, func(ctx context.Context, d *db.DB) error {
		for _, sub := range others {
			if err := endSubscription(ctx, d, s.Lifecycle, sub, models.CancelTypeChargeback, false, cause); err != nil {
				return err
			}
		}
		return done(ctx, d)
	})
}

// endCharge ends what a charge paid for, at once with its access, blocks its
// card and cancels at period end the subscriptions a provider bills on it.
func (s *Service) endCharge(ctx context.Context, d *db.DB, payment *models.Payment, own, others []uuid.UUID, cancelType models.CancelType, revoke models.AccessRevokeReason, cause string, review *gen.BillingDispute) error {
	for _, sub := range own {
		if err := endSubscription(ctx, d, s.Lifecycle, sub, cancelType, true, cause); err != nil {
			return err
		}
	}
	if payment.SubscriptionID == nil {
		if err := entitlements.NewEntitlementService(d, s.Clock).EndActiveByPayment(ctx, payment.ID, revoke); err != nil {
			return fmt.Errorf("end the charge's access: %w", err)
		}
	}
	if err := s.BlockCard(ctx, d, payment, review); err != nil {
		return err
	}
	for _, sub := range others {
		if err := endSubscription(ctx, d, s.Lifecycle, sub, cancelType, false, cause); err != nil {
			return err
		}
	}
	return nil
}

// admitted runs effect in one transaction with the command's outcome, once
// the kill switch, a full write posture and the automation budget admit it;
// otherwise it answers a WaitError. An effect that rolled back changed
// nothing and releases its units.
func (s *Service) admitted(ctx context.Context, command destructive.Command, effect func(context.Context, *db.DB) error) error {
	if s.Admissions == nil || s.Postures == nil || s.Lifecycle == nil {
		return &WaitError{Reason: "responses are not wired"}
	}
	if v := destructive.New(s.DB).CheckMerchant(ctx, command.MerchantID); !v.Allowed {
		return &WaitError{Reason: v.Reason}
	}
	if p := s.Postures.Posture(ctx, command.MerchantID); p.Limited() {
		return &WaitError{Reason: p.Reason}
	}
	action, err := s.Admissions.AdmitAutonomous(ctx, command, responseSource)
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
