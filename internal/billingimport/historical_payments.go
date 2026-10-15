package billingimport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const historicalPaymentSource = "import_payment"

// HistoricalPaymentFailpointKind identifies the atomic receipt import in tests.
const HistoricalPaymentFailpointKind = "historical_payment_import"

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
		case p.Rail != "nmi":
			reason = "historical payments currently support only NMI"
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
		if reason == "" {
			p.Refunds = append([]billing.DeclaredHistoricalRefund(nil), p.Refunds...)
			reason = validateHistoricalRefunds(&p, asOf)
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
			d := database.NewWithPgxTx(tx)
			// Imports with and without refunds take the same lock order. A
			// host may declare history before this customer's first visit.
			if err := db.EnsureCustomerRowQ(ctx, d.Gen(ctx), merchantID, p.decl.Customer.UUID()); err != nil {
				return err
			}
			if _, err := d.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: p.decl.Customer.UUID()}); err != nil {
				return err
			}
			created, err = recordHistoricalPayment(ctx, d, merchantID, p, now)
			if err != nil || len(p.decl.Refunds) == 0 {
				return err
			}
			refunded, err := recordHistoricalRefunds(ctx, d, merchantID, p, now)
			created = created || refunded
			return err
		})
		if err == nil {
			err = failpoint.Hit(ctx, failpoint.Site{Point: failpoint.Committed, Kind: HistoricalPaymentFailpointKind})
		}
		switch {
		case errors.Is(err, observed.ErrConflict), db.IsUniqueViolation(err):
			res.Blocked = append(res.Blocked, p.decl.SourceID)
			res.Reasons[p.decl.SourceID] = "the provider transaction is already recorded with different customer or payment facts"
		case errors.Is(err, payments.ErrRefundReservationPending):
			res.Blocked = append(res.Blocked, p.decl.SourceID)
			res.Reasons[p.decl.SourceID] = "a refund of this payment is still pending; retry after it resolves"
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

func validateHistoricalRefunds(p *billing.DeclaredHistoricalPayment, asOf time.Time) string {
	remaining := p.Amount
	seen := map[string]bool{}
	for i := range p.Refunds {
		r := &p.Refunds[i]
		if !providerTransactionID(p.Rail, r.TransactionID) || len(r.TransactionID) > 512 || r.TransactionID == p.TransactionID || seen[r.TransactionID] {
			return "refunds require distinct provider transaction IDs different from the original payment"
		}
		seen[r.TransactionID] = true
		if r.Amount <= 0 || r.Amount > remaining {
			return "refund amounts must be positive and their total cannot exceed the payment"
		}
		if _, err := moneyutil.NativeToRailMinorExact(p.Currency, r.Amount); err != nil {
			return err.Error()
		}
		remaining -= r.Amount
		if r.RefundedAt.IsZero() || r.RefundedAt.Before(p.PaidAt) || r.RefundedAt.After(asOf) {
			return "refund date must be between the payment date and as_of"
		}
		r.RefundedAt = r.RefundedAt.UTC().Truncate(time.Microsecond)
	}
	return ""
}

func recordHistoricalRefunds(ctx context.Context, d *db.DB, merchantID uuid.UUID, p historicalPayment, now time.Time) (bool, error) {
	ctx = db.WithPSPID(ctx, p.psp)
	svc := payments.NewPaymentService(d, clockwork.NewFakeClockAt(now))
	original, err := svc.GetByPSPTransactionID(ctx, models.Rail(p.decl.Rail), p.decl.TransactionID)
	if err != nil {
		return false, err
	}
	// Only our access-free historical orders can acquire refunds through this
	// path. Ordinary purchases may carry credits or subscription effects.
	bill, err := d.Gen(ctx).GetProviderBill(ctx, gen.GetProviderBillParams{MerchantID: merchantID, PspID: p.psp, SourceKind: historicalPaymentSource, SourceKey: p.decl.TransactionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, observed.ErrConflict
	}
	if err != nil {
		return false, err
	}
	if original.OrderID == nil || *original.OrderID != bill.OrderID || original.SubscriptionID != nil || original.LedgerTransferID != nil {
		return false, observed.ErrConflict
	}
	if _, err := d.Gen(ctx).LockPaymentForRefund(ctx, gen.LockPaymentForRefundParams{MerchantID: merchantID, PaymentID: original.ID}); err != nil {
		return false, err
	}
	existing, err := payments.NewPaymentRepo(d).ListRefunds(ctx, original.ID)
	if err != nil {
		return false, err
	}
	var total int64
	for _, r := range existing {
		if r.Status == payments.PaymentStatusPendingValue {
			return false, payments.ErrRefundReservationPending
		}
		if r.Status == payments.PaymentStatusSucceededValue && r.Amount < 0 {
			total -= r.Amount
		}
	}
	var missing []billing.DeclaredHistoricalRefund
	for _, r := range p.decl.Refunds {
		prior, err := svc.GetByPSPTransactionID(ctx, original.Rail, r.TransactionID)
		if errors.Is(err, pgx.ErrNoRows) {
			if r.Amount > original.Amount-total {
				return false, observed.ErrConflict
			}
			total += r.Amount
			missing = append(missing, r)
			continue
		}
		if err != nil {
			return false, err
		}
		if prior.RefundedPaymentID == nil || *prior.RefundedPaymentID != original.ID || prior.CustomerID != original.CustomerID || prior.Amount != -r.Amount || prior.Currency != original.Currency || prior.ReversalKind == nil || *prior.ReversalKind != payments.ReversalRefund || prior.Status != payments.PaymentStatusSucceededValue || !prior.PurchasedAt.Equal(r.RefundedAt) {
			return false, observed.ErrConflict
		}
	}
	for _, r := range missing {
		if _, err := svc.RecordObservedRefund(ctx, original.ID, r.TransactionID, r.Amount, payments.ReversalRefund, r.RefundedAt, nil); err != nil {
			return false, err
		}
		if err := failpoint.Hit(ctx, failpoint.Site{Point: failpoint.AfterAttempt, Kind: HistoricalPaymentFailpointKind}); err != nil {
			return false, err
		}
	}
	return len(missing) > 0, nil
}
