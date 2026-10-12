package subscriptions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
)

func initialBill(ctx context.Context, d *db.DB, params *CreateMembershipParams, sub *models.Subscription, p *models.Payment, now time.Time) (observed.Result, error) {
	if params.ProviderBill != nil {
		b := *params.ProviderBill
		b.SubscriptionID = &sub.ID
		b.Payment = p
		return observed.Record(ctx, d, b, now)
	}
	t := params.Prepared
	if t == nil {
		return observed.Result{}, errors.New("provider enrollment payment requires verified bill evidence")
	}
	interval := int(t.PeriodEnd.Sub(t.PeriodStart) / time.Hour)
	if t.RecurringIntervalHours != nil {
		interval = *t.RecurringIntervalHours
	}
	b := observed.Bill{PSPID: t.PSPID, CustomerID: t.CustomerID, SubscriptionID: &t.SubscriptionID, Owner: string(t.CollectionPolicy), Kind: "initial", SourceKind: "accepted_initial", SourceKey: t.SubscriptionID.String(), Currency: t.Currency, Total: t.Amount, PeriodStart: t.PeriodStart, PeriodEnd: t.PeriodEnd, DueAt: t.PeriodStart, Payment: p, Settled: true, SettlementSource: "cash", Lines: []observed.Line{{PriceID: &t.PriceID, ProductID: &t.ProductID, Description: t.ProductName, Quantity: t.Quantity, UnitAmount: t.Amount / int64(SeatCount(t.Quantity)), Amount: t.Amount, BillingIntervalHours: &interval, AccessDurationHours: t.AccessDurationHours}}}
	if b.Lines[0].Description == "" {
		b.Lines[0].Description = "Subscription enrollment"
	}
	b.ExistingOrderID = params.ExistingOrderID
	recorded, err := observed.Record(ctx, d, b, now)
	if err != nil {
		return recorded, err
	}
	if t.CollectionPolicy != models.CollectionPolicyEngine {
		if t.RecurringIntervalHours == nil {
			return recorded, errors.New("accepted provider enrollment has no recurring cadence")
		}
		firstDue := t.PeriodEnd
		if t.Pending {
			firstDue = t.PeriodStart
		}
		if err := observed.SeedSchedule(ctx, d, recorded.Order.ID, sub.ID, firstDue, *t.RecurringIntervalHours, t.RecurringAmount, now); err != nil {
			return recorded, err
		}
	}
	return recorded, nil
}

func renewalBill(ctx context.Context, d *db.DB, params *RenewMembershipParams, sub *models.Subscription, p *models.Payment, now time.Time) (observed.Result, error) {
	if params.ProviderBill != nil {
		b := *params.ProviderBill
		if p != nil {
			b.Payment = p
		}
		return observed.Record(ctx, d, b, now)
	}
	t := params.Prepared
	if t == nil {
		return observed.Result{}, errors.New("provider renewal payment requires verified bill evidence")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return observed.Result{}, err
	}
	boundary := t.PeriodStart
	if params.PreviousPeriodEnd != nil {
		boundary = *params.PreviousPeriodEnd
	}
	order, err := d.Gen(ctx).GetRenewalOrder(ctx, gen.GetRenewalOrderParams{MerchantID: mid.UUID(), SubscriptionID: t.SubscriptionID, PeriodStartsAt: boundary})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return observed.Result{}, err
	}
	interval := int(t.PeriodEnd.Sub(t.PeriodStart) / time.Hour)
	b := observed.Bill{PSPID: t.PSPID, CustomerID: t.CustomerID, SubscriptionID: &t.SubscriptionID, Owner: string(sub.CollectionPolicy), Kind: "renewal", SourceKind: "accepted_renewal", SourceKey: t.SubscriptionID.String() + ":" + boundary.UTC().Format(time.RFC3339Nano), Currency: t.Currency, Total: t.Amount, PeriodStart: t.PeriodStart, PeriodEnd: t.PeriodEnd, DueAt: boundary, Payment: p, Lines: []observed.Line{{PriceID: &t.PriceID, ProductID: &t.ProductID, Description: t.ProductName, Quantity: t.Quantity, UnitAmount: t.UnitAmount(), Amount: t.Amount, BillingIntervalHours: &interval, AccessDurationHours: t.AccessDurationHours}}}
	if b.Lines[0].Description == "" {
		b.Lines[0].Description = "Subscription renewal"
	}
	if err == nil {
		b.ExistingOrderID = &order.ID
	}
	return observed.Record(ctx, d, b, now)
}

func paymentHasPrice(ctx context.Context, d *db.DB, p *models.Payment, price uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	matches, err := d.Gen(ctx).PaymentMatchesOrderPrice(ctx, gen.PaymentMatchesOrderPriceParams{MerchantID: mid.UUID(), PaymentID: p.ID, PriceID: price})
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("payment order belongs to another accepted price")
	}
	return nil
}
