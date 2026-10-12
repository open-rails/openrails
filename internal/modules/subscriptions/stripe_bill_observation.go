package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// StripeBillObservation translates an invoice's immutable lines and verified
// settlements. Catalog entries supply identities only: amounts, quantities and
// service periods always come from the invoice itself.
func StripeBillObservation(ctx context.Context, d *db.DB, sub *models.Subscription, invoice StripeBill) (observed.Bill, []*models.Payment, error) {
	bill := observed.Bill{}
	if sub == nil || sub.Rail != models.RailStripe || sub.CollectionPolicy != models.CollectionPolicyProvider || invoice.SubscriptionID != sub.RailSubscriptionID {
		return bill, nil, errors.New("Stripe invoice does not belong to the provider-owned subscription")
	}
	bill = observed.Bill{PSPID: sub.PspID, CustomerID: sub.CustomerID, SubscriptionID: &sub.ID,
		Owner: observed.OwnerProvider, Kind: "renewal", SourceKind: "stripe_invoice", SourceKey: invoice.ID,
		ProviderReference: invoice.ID, Currency: invoice.Currency, DueAt: invoice.DueAt}
	switch invoice.Reason {
	case "subscription_create":
		bill.Kind = "initial"
	case "subscription_cycle":
	case "subscription_update", "subscription_threshold", "manual":
		bill.Kind = "adjustment"
	default:
		return bill, nil, fmt.Errorf("Stripe invoice %s has unsupported billing reason %q", invoice.ID, invoice.Reason)
	}
	if invoice.Total < 0 || invoice.AmountDue < 0 || invoice.AmountRemaining < 0 || invoice.AmountPaid < 0 || invoice.AmountOffStripe < 0 {
		return bill, nil, errors.New("Stripe invoice has unsupported negative billing amounts")
	}
	var err error
	if bill.Total, err = moneyutil.RailMinorToNative(invoice.Currency, moneyutil.Cents(invoice.Total)); err != nil {
		return bill, nil, err
	}
	var lineTotal int64
	for _, line := range invoice.Lines {
		amount, err := moneyutil.RailMinorToNative(invoice.Currency, moneyutil.Cents(line.Amount))
		if err != nil {
			return bill, nil, err
		}
		if amount > 0 && lineTotal > math.MaxInt64-amount || amount < 0 && lineTotal < math.MinInt64-amount {
			return bill, nil, errors.New("Stripe invoice line total overflows")
		}
		lineTotal += amount
		frozen := observed.Line{Description: line.Description, Quantity: line.Quantity, Amount: amount,
			UnitAmount: amount, Source: "provider_adjustment", ProviderReference: line.ID}
		if line.Quantity != nil {
			frozen.UnitAmount = amount / int64(*line.Quantity)
		}
		if line.PriceID != "" && amount >= 0 {
			price, err := catalog.NewPriceService(d).GetByStripePriceID(ctx, line.PriceID)
			if err != nil {
				return bill, nil, fmt.Errorf("Stripe invoice %s line %s price %s is unresolved: %w", invoice.ID, line.ID, line.PriceID, err)
			}
			frozen.PriceID, frozen.ProductID, frozen.Source = &price.ID, &price.ProductID, ""
			// This is a historical fact, not an instruction to grant today's
			// product or to apply today's access duration.
		}
		if frozen.UnitAmount < 0 {
			frozen.UnitAmount = 0
		}
		if frozen.Description == "" {
			frozen.Description = "Stripe invoice line"
		}
		bill.Lines = append(bill.Lines, frozen)
		if !line.PeriodStart.IsZero() && line.PeriodEnd.After(line.PeriodStart) {
			if bill.PeriodStart.IsZero() || line.PeriodStart.Before(bill.PeriodStart) {
				bill.PeriodStart = line.PeriodStart
			}
			if line.PeriodEnd.After(bill.PeriodEnd) {
				bill.PeriodEnd = line.PeriodEnd
			}
		}
	}
	if len(bill.Lines) == 0 {
		return bill, nil, errors.New("Stripe invoice has no attributable billing lines")
	}
	// Stripe's total includes explicit invoice-level discounts, taxes and
	// shipping not present in line.amount. Preserve that exact adjustment;
	// never multiply a rounded seat price to replace the provider total.
	if lineTotal != bill.Total {
		if lineTotal < 0 && bill.Total > math.MaxInt64+lineTotal {
			return bill, nil, errors.New("Stripe invoice adjustment overflows")
		}
		adjustment := bill.Total - lineTotal
		bill.Lines = append(bill.Lines, observed.Line{Source: "provider_adjustment", Description: "Stripe invoice tax, discount and shipping adjustment",
			Amount: adjustment, UnitAmount: max(int64(0), adjustment), ProviderReference: invoice.ID + ":adjustment"})
	}
	if bill.Kind != "adjustment" && (bill.PeriodStart.IsZero() || !bill.PeriodEnd.After(bill.PeriodStart)) {
		return bill, nil, errors.New("Stripe subscription invoice has no verified service period")
	}
	var receipts []*models.Payment
	var cash int64
	seen := map[string]bool{}
	for _, receipt := range invoice.Payments {
		if receipt.Status != "paid" || receipt.Kind == "payment_record" {
			continue
		}
		if receipt.ChargeID == "" || receipt.Amount <= 0 || !strings.EqualFold(receipt.Currency, invoice.Currency) || seen[receipt.ChargeID] {
			return bill, nil, errors.New("Stripe invoice contains conflicting monetary allocations")
		}
		seen[receipt.ChargeID] = true
		amount, err := moneyutil.RailMinorToNative(invoice.Currency, moneyutil.Cents(receipt.Amount))
		if err != nil || cash > math.MaxInt64-amount {
			return bill, nil, errors.New("Stripe invoice payment amount is not representable")
		}
		cash += amount
		at := receipt.PaidAt
		if at.IsZero() {
			return bill, nil, errors.New("Stripe invoice payment has no verified payment time")
		}
		receipts = append(receipts, &models.Payment{ID: uuidutil.NewV7(), CustomerID: sub.CustomerID, SubscriptionID: &sub.ID,
			PspID: &sub.PspID, Rail: models.RailStripe, TransactionID: receipt.ChargeID, Amount: amount, ListAmount: amount,
			Currency: invoice.Currency, Status: payments.PaymentStatusSucceededValue, MoneyMovement: models.MoneyMovementRail,
			PurchasedAt: at, CreatedAt: at, Metadata: map[string]any{"stripe_invoice_id": invoice.ID,
				"stripe_invoice_payment_id": receipt.ID, "stripe_payment_intent_id": receipt.PaymentIntentID}})
	}
	if invoice.Status == "paid" && invoice.AmountRemaining == 0 {
		bill.Settled = true
		cashMinor, err := moneyutil.NativeToRailMinorExact(invoice.Currency, cash)
		if err != nil {
			return bill, nil, err
		}
		// A paid flag is not gateway cash. Require the remainder to be
		// explained by an explicit off-Stripe amount or credit application.
		credit := invoice.PrePaymentCredit
		if invoice.StartingBalance < 0 {
			if invoice.StartingBalance == math.MinInt64 || credit > math.MaxInt64+invoice.StartingBalance {
				return bill, nil, errors.New("Stripe invoice credit overflows")
			}
			credit -= invoice.StartingBalance
		}
		if int64(cashMinor) > math.MaxInt64-invoice.AmountOffStripe || int64(cashMinor)+invoice.AmountOffStripe < invoice.AmountPaid {
			return bill, nil, errors.New("Stripe paid invoice has unresolved settlement evidence")
		}
		remaining := max(int64(0), invoice.Total-int64(cashMinor))
		if invoice.AmountOffStripe > math.MaxInt64-credit || invoice.AmountOffStripe+credit < remaining {
			return bill, nil, errors.New("Stripe invoice total is not covered by verified settlement")
		}
		if bill.NonCashAmount, err = moneyutil.RailMinorToNative(invoice.Currency, moneyutil.Cents(remaining)); err != nil {
			return bill, nil, err
		}
		switch {
		case remaining == 0:
			bill.SettlementSource = "cash"
			if bill.Total == 0 {
				bill.SettlementSource = "cash"
			}
		case cash > 0 || invoice.AmountOffStripe > 0 && credit > 0:
			bill.SettlementSource = "mixed"
		case invoice.AmountOffStripe > 0:
			bill.SettlementSource = "out_of_band"
		default:
			bill.SettlementSource = "credit"
		}
	}
	return bill, receipts, nil
}

// RecordStripeBill writes all real movements and the settlement transition in
// the caller's transaction. It never calls a provider or fulfills a product.
func RecordStripeBill(ctx context.Context, d *db.DB, sub *models.Subscription, invoice StripeBill, now time.Time) (observed.Result, error) {
	bill, receipts, err := StripeBillObservation(ctx, d, sub, invoice)
	if err != nil {
		return observed.Result{}, err
	}
	if bill.Kind == "initial" {
		if id := invoice.Metadata["order_id"]; id != "" {
			parsed, err := uuid.Parse(id)
			if err != nil {
				return observed.Result{}, errors.New("Stripe initial invoice has invalid accepted order identity")
			}
			bill.ExistingOrderID = &parsed
		}
	}
	settled, source, noncash := bill.Settled, bill.SettlementSource, bill.NonCashAmount
	bill.Settled, bill.SettlementSource, bill.NonCashAmount = false, "", 0
	var result observed.Result
	var created, paymentCreated, completed bool
	for _, receipt := range receipts {
		bill.Payment = receipt
		if result, err = observed.Record(ctx, d, bill, now); err != nil {
			return result, err
		}
		created = created || result.Created
		paymentCreated = paymentCreated || result.PaymentCreated
		completed = completed || result.Completed
	}
	bill.Settled, bill.SettlementSource, bill.NonCashAmount = settled, source, noncash
	result, err = observed.Record(ctx, d, bill, now)
	result.Created = result.Created || created
	result.PaymentCreated = result.PaymentCreated || paymentCreated
	result.Completed = result.Completed || completed
	return result, err
}
