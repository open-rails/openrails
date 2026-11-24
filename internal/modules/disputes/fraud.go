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
	"github.com/open-rails/openrails/internal/disputejob"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
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
// false for a report already recorded. A report stays actionable only while
// every report of it says so. A new report queues its PSP's monitor. One the
// merchant refunds is pending, decided by its response, when it is
// actionable and the merchant refunds reports as it arrives; a merchant who
// later starts refunding leaves the reports recorded before alone.
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
	ref := strings.TrimSpace(r.ProviderRef)
	action := fraudRecorded
	if r.Actionable {
		refunds, err := merchantconfig.FraudWarningRefunds(ctx, d)
		if err != nil {
			return gen.BillingFraudReport{}, false, err
		}
		if refunds {
			action = fraudPending
		}
	}
	var out gen.BillingFraudReport
	added := false
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := d.NewWithPgxTx(tx)
		q := txDB.Gen(ctx)
		row, err := q.InsertFraudReport(ctx, gen.InsertFraudReportParams{
			MerchantID: mid.UUID(), PspID: r.PSPID, Rail: string(r.Rail), ProviderRef: ref,
			CustomerID: r.Payment.CustomerID, PaymentID: r.Payment.ID, Network: string(disputerules.Network(models.DerefStr(r.Payment.CardBrand))),
			FraudType: kind, Actionable: r.Actionable, Amount: r.Payment.Amount, Currency: r.Payment.Currency, ReportedAt: orNow(r.ReportedAt, s.now()),
			Action: action,
		})
		switch {
		case err == nil:
			out, added = row, true
			if err := disputejob.QueueMonitor(ctx, txDB, mid.UUID(), r.PSPID); err != nil {
				return err
			}
		case errors.Is(err, pgx.ErrNoRows):
			existing, err := q.GetFraudReportForUpdate(ctx, gen.GetFraudReportForUpdateParams{MerchantID: mid.UUID(), PspID: r.PSPID, ProviderRef: ref})
			if err != nil {
				return fmt.Errorf("lock fraud report %s: %w", ref, err)
			}
			if out, err = q.NoteFraudReportActionable(ctx, gen.NoteFraudReportActionableParams{MerchantID: mid.UUID(), ID: existing.ID, Actionable: r.Actionable}); err != nil {
				return fmt.Errorf("update fraud report %s: %w", ref, err)
			}
		default:
			return fmt.Errorf("record fraud report %s: %w", ref, err)
		}
		if !r.Actionable || !waitingReport(out) {
			return nil
		}
		return disputejob.QueueResponse(ctx, txDB, mid.UUID(), disputejob.SubjectFraudReport, out.ID)
	})
	return out, added, err
}

// waitingReport reports whether a report's response has yet to finish.
func waitingReport(r gen.BillingFraudReport) bool {
	return r.RespondedAt == nil && (r.Action == fraudPending || r.Action == fraudRefunded || r.Action == fraudNothingToRefund)
}

// What OpenRails decided about a fraud report.
const (
	fraudPending         = "pending"
	fraudRecorded        = "recorded"
	fraudRefunded        = "refunded"
	fraudNothingToRefund = "nothing_to_refund"
	fraudDisputed        = "disputed"
)
