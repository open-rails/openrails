package billingimport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const historicalPaymentSource = "import_payment"

type historicalPayment struct {
	decl billing.DeclaredHistoricalPayment
	psp  uuid.UUID
}

func planHistoricalPayments(psps *pspResolver, declared []billing.DeclaredHistoricalPayment, asOf time.Time, res *Result) ([]historicalPayment, error) {
	var out []historicalPayment
	seen := map[string]bool{}
	for _, p := range declared {
		if strings.TrimSpace(p.SourceID) == "" || seen[p.SourceID] {
			return nil, apperr.Invalidf("historical payment source_id must be present and unique").WithParam("historical_payments")
		}
		seen[p.SourceID] = true
		p.Rail = strings.ToLower(strings.TrimSpace(p.Rail))
		p.Currency = moneyutil.NormalizeCurrency(p.Currency)
		psp, err := psps.resolve(p.PSP, p.Rail, "historical payment "+p.SourceID)
		if err != nil {
			return nil, err
		}
		reason := ""
		switch {
		case p.Customer.IsZero():
			reason = "customer is required"
		case p.Rail != "nmi" && p.Rail != "ccbill" && p.Rail != "stripe":
			reason = "historical payments require a supported fiat provider"
		case !providerTransactionID(p.Rail, p.TransactionID) || len(p.TransactionID) > 512:
			reason = "a valid provider transaction_id is required"
		case p.Amount <= 0 || moneyutil.RequireFiatCurrency(p.Currency) != nil:
			reason = "a positive amount and known fiat currency are required"
		case p.PaidAt.IsZero() || p.PaidAt.After(asOf):
			reason = "paid_at is required and cannot be after as_of"
		default:
			if _, err := moneyutil.NativeToRailMinorExact(p.Currency, p.Amount); err != nil {
				reason = err.Error()
			}
		}
		if reason != "" {
			res.Blocked = append(res.Blocked, p.SourceID)
			res.Reasons[p.SourceID] = reason
			continue
		}
		p.PaidAt = p.PaidAt.UTC().Truncate(time.Microsecond)
		out = append(out, historicalPayment{decl: p, psp: psp})
	}
	return out, nil
}

// Each receipt commits its order and payment together. Replaying after a lost
// response cannot create a second order or move the payment to another customer.
func recordHistoricalPayments(ctx context.Context, database *db.DB, merchantID uuid.UUID, planned []historicalPayment, now time.Time, res *Result) error {
	for _, p := range planned {
		created := false
		err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = recordHistoricalPayment(ctx, database.NewWithPgxTx(tx), merchantID, p, now)
			return err
		})
		switch {
		case errors.Is(err, observed.ErrConflict):
			res.Blocked = append(res.Blocked, p.decl.SourceID)
			res.Reasons[p.decl.SourceID] = "the provider transaction is already recorded with different customer or payment facts"
		case err != nil:
			return fmt.Errorf("record historical payment %s: %w", p.decl.SourceID, err)
		case created:
			res.Imported = append(res.Imported, p.decl.SourceID)
		default:
			res.Skipped = append(res.Skipped, p.decl.SourceID)
		}
	}
	return nil
}

func recordHistoricalPayment(ctx context.Context, d *db.DB, merchantID uuid.UUID, p historicalPayment, now time.Time) (bool, error) {
	q := d.Gen(ctx)
	// Use the provider transaction, not the import job or staging record, as
	// identity so separate imports of the same receipt serialize as well.
	if err := q.LockProviderBillIdentity(ctx, gen.LockProviderBillIdentityParams{MerchantID: merchantID.String(), PspID: p.psp.String(), SourceKind: historicalPaymentSource, SourceKey: p.decl.TransactionID}); err != nil {
		return false, err
	}
	rail := p.decl.Rail
	prior, err := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: merchantID, PspID: &p.psp, Channel: string(models.ChannelRail), Rail: &rail, TransactionID: p.decl.TransactionID})
	if err == nil {
		if prior.CustomerID != p.decl.Customer.UUID() || prior.Amount != p.decl.Amount || prior.Currency != p.decl.Currency ||
			!prior.PurchasedAt.Equal(p.decl.PaidAt) || prior.RefundedPaymentID != nil ||
			prior.MoneyMovement != string(models.MoneyMovementRail) || prior.Status != "succeeded" && prior.Status != "refunded" {
			return false, observed.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	result, err := observed.Record(ctx, d, observed.Bill{
		PSPID: p.psp, CustomerID: p.decl.Customer.UUID(), Owner: observed.OwnerProvider, Kind: "purchase",
		SourceKind: historicalPaymentSource, SourceKey: p.decl.TransactionID, ProviderReference: p.decl.TransactionID,
		Currency: p.decl.Currency, Total: p.decl.Amount, DueAt: p.decl.PaidAt,
		Lines: []observed.Line{{Description: "Historical payment", Source: historicalPaymentSource, UnitAmount: p.decl.Amount, Amount: p.decl.Amount}},
		Payment: &models.Payment{CustomerID: p.decl.Customer.UUID(), Channel: models.ChannelRail, Rail: models.Rail(p.decl.Rail),
			TransactionID: p.decl.TransactionID, Amount: p.decl.Amount, ListAmount: p.decl.Amount, Currency: p.decl.Currency,
			Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail, PurchasedAt: p.decl.PaidAt, CreatedAt: p.decl.PaidAt},
		Settled: true, SettlementSource: "cash",
	}, now)
	return result.PaymentCreated, err
}
