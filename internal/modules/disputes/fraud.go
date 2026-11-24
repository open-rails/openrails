package disputes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
)

// FraudReport is an issuer's fraud report a rail passed on.
type FraudReport struct {
	PSPID       uuid.UUID
	Rail        models.Rail
	ProviderRef string
	Payment     *models.Payment
	FraudType   string
	Actionable  bool
	ReportedAt  time.Time
}

var fraudTypes = map[string]bool{"card_never_received": true, "fraudulent_card_application": true, "made_with_counterfeit_card": true,
	"made_with_lost_card": true, "made_with_stolen_card": true, "misc": true, "unauthorized_use_of_card": true}

// RecordFraudReport writes the report once, in d's transaction; added is
// false for a report already recorded. A new report queues its PSP's monitor.
func (s *Service) RecordFraudReport(ctx context.Context, d *db.DB, r FraudReport) (gen.BillingFraudReport, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingFraudReport{}, false, err
	}
	if r.Payment == nil || r.PSPID == uuid.Nil || strings.TrimSpace(r.ProviderRef) == "" {
		return gen.BillingFraudReport{}, false, errors.New("fraud report: charge, PSP and provider reference are required")
	}
	kind := strings.ToLower(strings.TrimSpace(r.FraudType))
	if !fraudTypes[kind] {
		kind = "misc"
	}
	var out gen.BillingFraudReport
	added := false
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := d.NewWithPgxTx(tx)
		q := txDB.Gen(ctx)
		row, err := q.InsertFraudReport(ctx, gen.InsertFraudReportParams{
			MerchantID: mid.UUID(), PspID: r.PSPID, Rail: string(r.Rail), ProviderRef: strings.TrimSpace(r.ProviderRef),
			CustomerID: r.Payment.CustomerID, PaymentID: r.Payment.ID, Network: string(disputerules.Network(models.DerefStr(r.Payment.CardBrand))),
			FraudType: kind, Actionable: r.Actionable, Amount: r.Payment.Amount, Currency: r.Payment.Currency, ReportedAt: orNow(r.ReportedAt, s.now()),
		})
		switch {
		case err == nil:
			out, added = row, true
			return QueueMonitor(ctx, txDB, mid, r.PSPID)
		case errors.Is(err, pgx.ErrNoRows):
			out, err = q.GetFraudReportForUpdate(ctx, gen.GetFraudReportForUpdateParams{MerchantID: mid.UUID(), PspID: r.PSPID, ProviderRef: strings.TrimSpace(r.ProviderRef)})
			return err
		}
		return fmt.Errorf("record fraud report %s: %w", r.ProviderRef, err)
	})
	return out, added, err
}

// MarkFraudReportRefunded records the refund that answered the report.
func (s *Service) MarkFraudReportRefunded(ctx context.Context, d *db.DB, report gen.BillingFraudReport, refund uuid.UUID) error {
	if _, err := d.Gen(ctx).MarkFraudReportRefunded(ctx, gen.MarkFraudReportRefundedParams{MerchantID: report.MerchantID, ID: report.ID, RefundPaymentID: refund}); err != nil {
		return fmt.Errorf("mark fraud report %s refunded: %w", report.ProviderRef, err)
	}
	return nil
}
