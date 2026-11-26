package webhooks

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/disputejob"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/disputes"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// AttachNMIChargeback books an unmatched case against the charge the operator
// identified and ends what it bought, as a matched provider notice does.
func (d *WebhookDispatcher) AttachNMIChargeback(ctx context.Context, disputeID uuid.UUID, paymentID uuid.UUID, actor, reason string) error {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	if actor == "" || actor == "unknown" || reason == "" {
		return errors.New("actor and reason are required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.DB.Gen(ctx)
	dispute, err := q.GetDispute(ctx, gen.GetDisputeParams{MerchantID: mid.UUID(), ID: disputeID})
	if err != nil {
		return fmt.Errorf("load dispute: %w", err)
	}
	if dispute.Kind != string(billing.DisputeChargeback) || dispute.Rail != string(models.RailNMI) {
		return errors.New("dispute must be an NMI chargeback")
	}
	// A rerun for the same charge resumes an attach whose effects did not all
	// land; they are idempotent.
	resume := dispute.PaymentID != nil
	if resume && (*dispute.PaymentID != paymentID || dispute.ReversalPaymentID != nil) {
		return errors.New("dispute is already attached")
	}
	payment, err := d.PaymentService.GetByID(ctx, paymentID)
	if err != nil {
		return fmt.Errorf("load payment: %w", err)
	}
	if payment.PspID == nil || *payment.PspID != dispute.PspID || payment.Rail != models.RailNMI ||
		payment.Status != payments.PaymentStatusSucceededValue || payment.Amount <= 0 || payment.MoneyMovement != models.MoneyMovementRail ||
		payment.RefundedPaymentID != nil || payment.ReversalKind != nil {
		return errors.New("payment must be a succeeded NMI charge with rail money movement on the dispute's PSP")
	}
	match := &nmiChargebackMatch{
		PaymentID: payment.ID, PaymentTransactionID: payment.TransactionID,
		UserID: payment.CustomerID.String(), Amount: payment.Amount, Currency: payment.Currency, PurchasedAt: payment.PurchasedAt,
	}
	if payment.CardLast4 != nil {
		match.CardLast4 = *payment.CardLast4
	}
	if payment.SubscriptionID != nil {
		sub, err := d.SubscriptionService.GetByID(ctx, *payment.SubscriptionID)
		if err != nil {
			return fmt.Errorf("load payment subscription: %w", err)
		}
		match.SubscriptionID, match.RailSubscriptionID = sub.ID, sub.RailSubscriptionID
	}
	ctx = db.WithPSPID(ctx, dispute.PspID)
	if !resume {
		if err := d.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			txDB := d.DB.NewWithPgxTx(tx)
			var err error
			dispute, err = txDB.Gen(ctx).AttachDisputePayment(ctx, gen.AttachDisputePaymentParams{
				MerchantID: mid.UUID(), ID: disputeID, PaymentID: paymentID, CustomerID: payment.CustomerID,
				Amount: payment.Amount, Currency: payment.Currency,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("dispute is already attached")
			}
			if err != nil {
				return fmt.Errorf("attach dispute payment: %w", err)
			}
			return disputejob.QueueResponse(ctx, txDB, mid.UUID(), disputejob.SubjectDispute, disputeID)
		}); err != nil {
			return err
		}
	}
	entry := NMIChargebackEntry{ID: Stringish(dispute.ProviderRef), Reason: dispute.Reason}
	if dispute.ReasonCode != nil {
		entry.ReasonCode = *dispute.ReasonCode
	}
	if dispute.ProviderReason != nil {
		entry.Reason = *dispute.ProviderReason
	}
	service := d.nmiWebhookService()
	var effects nmiChargebackEffects
	if err := service.applyNMIChargeback(ctx, string(models.RailNMI), entry, match, &disputes.Case{BillingDispute: dispute}, map[string]any{}, map[uuid.UUID]struct{}{}, &effects); err != nil {
		return err
	}
	if err := errors.Join(effects.ledgerAlertErr, effects.terminationErr); err != nil {
		return fmt.Errorf("apply attached NMI chargeback: %w", err)
	}
	finding, err := q.GetPSPFindingByIdentity(ctx, gen.GetPSPFindingByIdentityParams{
		MerchantID: mid.UUID(), FindingType: string(alerting.FindingLedgerUnbooked),
		SubjectKey: unmatchedChargebackKey(dispute.ProviderRef, dispute.PspID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load unmatched chargeback finding: %w", err)
	}
	_, err = q.AdminResolveReconciliationFinding(ctx, gen.AdminResolveReconciliationFindingParams{ID: finding.ID, ResolvedBy: &actor, OperatorNotes: &reason})
	if err != nil {
		return fmt.Errorf("resolve unmatched chargeback finding: %w", err)
	}
	return nil
}
