package webhooks

import (
	"context"
	"encoding/json"
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
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/modules/disputes"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
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
	if resume && *dispute.PaymentID != paymentID {
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
	finding, err := q.GetPSPFindingByIdentity(ctx, gen.GetPSPFindingByIdentityParams{
		MerchantID: mid.UUID(), FindingType: string(alerting.FindingLedgerUnbooked),
		SubjectKey: unmatchedChargebackKey(dispute.ProviderRef, dispute.PspID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("no unmatched chargeback finding records this entry")
	}
	if err != nil {
		return fmt.Errorf("load unmatched chargeback finding: %w", err)
	}
	var evidence struct {
		Metadata struct {
			Amount string `json:"amount"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(finding.Evidence, &evidence); err != nil {
		return fmt.Errorf("read unmatched chargeback evidence: %w", err)
	}
	currencies, amounts := nmiChargebackReadings(evidence.Metadata.Amount)
	equal := false
	for i, currency := range currencies {
		if currency == payment.Currency && amounts[i] == payment.Amount {
			equal = true
			break
		}
	}
	if !equal {
		return fmt.Errorf("chargeback of %s does not equal charge %s; a partial chargeback is not booked by attach", evidence.Metadata.Amount, moneyutil.FormatAmount(payment.Amount, payment.Currency))
	}
	service := d.nmiWebhookService()
	match, err := service.nmiChargebackMatchFromPayment(ctx, payment)
	if err != nil {
		return err
	}
	network := billing.CardNetwork(dispute.Network)
	if network == billing.NetworkUnknown {
		network = disputerules.Network(models.DerefStr(payment.CardBrand))
	}
	classified := disputerules.Classify(network, models.DerefStr(dispute.ReasonCode), models.DerefStr(dispute.ProviderReason))
	ctx = db.WithPSPID(ctx, dispute.PspID)
	if !resume {
		if err := d.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			txDB := d.DB.NewWithPgxTx(tx)
			var err error
			dispute, err = txDB.Gen(ctx).AttachDisputePayment(ctx, gen.AttachDisputePaymentParams{
				MerchantID: mid.UUID(), ID: disputeID, PaymentID: paymentID, CustomerID: payment.CustomerID,
				Amount: payment.Amount, Currency: payment.Currency, Network: string(network), Reason: string(classified),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("dispute is already attached")
			}
			if err != nil {
				return fmt.Errorf("attach dispute payment: %w", err)
			}
			if err := disputejob.QueueResponse(ctx, txDB, mid.UUID(), disputejob.SubjectDispute, disputeID); err != nil {
				return err
			}
			return disputejob.QueueMonitor(ctx, txDB, mid.UUID(), dispute.PspID)
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
	var effects nmiChargebackEffects
	if err := service.applyNMIChargeback(ctx, string(models.RailNMI), entry, match, &disputes.Case{BillingDispute: dispute}, map[string]any{}, map[uuid.UUID]struct{}{}, &effects); err != nil {
		return err
	}
	if err := errors.Join(effects.ledgerAlertErr, effects.terminationErr); err != nil {
		return fmt.Errorf("apply attached NMI chargeback: %w", err)
	}
	_, err = q.AdminResolveReconciliationFinding(ctx, gen.AdminResolveReconciliationFindingParams{ID: finding.ID, ResolvedBy: &actor, OperatorNotes: &reason})
	if err != nil {
		return fmt.Errorf("resolve unmatched chargeback finding: %w", err)
	}
	return nil
}
