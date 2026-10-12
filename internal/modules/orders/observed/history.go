package observed

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// SourceHistory is the bill identity of a provider charge no accepted
// schedule cycle holds: one bill per provider transaction.
const SourceHistory = "provider_pre_schedule"

// History is where a provider receipt outside every accepted cycle belongs.
// A zero History leaves the receipt to ScheduleBill.
type History struct {
	// Bill is the settled bill of an approved charge, ready to record.
	Bill *Bill
	// Skip: a decline there moves no money, or the charge is already a
	// payment of another order.
	Skip bool
}

// HistoricalBill places an NMI or CCBill receipt made before the
// subscription's first accepted schedule cycle, or on a terminal subscription
// that has no accepted schedule. A recurring charge pays the cycle it starts,
// so the bill covers one accepted cadence from the charge (the price's,
// without a schedule) on the subscription's price, at the provider's amount.
// It binds no cycle, so it renews nothing, grants nothing and infers no
// schedule. A live subscription without an accepted schedule stays with
// ScheduleBill. Call it on a locked subscription in the caller's transaction.
func HistoricalBill(ctx context.Context, d *db.DB, sub *models.Subscription, receipt ScheduleReceipt) (History, error) {
	// Stripe's history is its finalized invoices; only schedule rails' charges
	// stand on their own.
	if sub == nil || receipt.TransactionID == "" || receipt.OccurredAt.IsZero() || sub.CollectionPolicy == models.CollectionPolicyEngine || sub.RailSubscriptionID == "" ||
		sub.Rail != models.RailNMI && sub.Rail != models.RailCCBill {
		return History{}, nil
	}
	q := d.Gen(ctx)
	if _, err := q.GetProviderCycleReceipt(ctx, gen.GetProviderCycleReceiptParams{MerchantID: sub.MerchantID, PspID: sub.PspID, TransactionID: receipt.TransactionID}); err == nil {
		return History{}, nil
	} else if !db.IsNotFound(err) {
		return History{}, err
	}
	// A recorded history bill replays as it was accepted.
	if accepted, err := q.GetProviderBill(ctx, gen.GetProviderBillParams{MerchantID: sub.MerchantID, PspID: sub.PspID, SourceKind: SourceHistory, SourceKey: receipt.TransactionID}); err == nil {
		var bill Bill
		if err := json.Unmarshal(accepted.Snapshot, &bill); err != nil {
			return History{}, err
		}
		return History{Bill: &bill}, nil
	} else if !db.IsNotFound(err) {
		return History{}, err
	}
	at := receipt.OccurredAt.UTC().Truncate(time.Microsecond)
	price, err := q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: sub.MerchantID, ID: sub.PriceID})
	if err != nil {
		return History{}, err
	}
	schedules, err := q.ListProviderSchedules(ctx, gen.ListProviderSchedulesParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, ProviderReference: sub.RailSubscriptionID})
	if err != nil {
		return History{}, err
	}
	var hours, months int
	switch {
	case len(schedules) > 0:
		first := schedules[0]
		if !at.Truncate(24 * time.Hour).Before(first.FirstDueAt.UTC().Truncate(24 * time.Hour)) {
			return History{}, nil
		}
		hours, months = int(first.CadenceHours), int(first.CadenceMonths)
	case sub.Status == models.StatusCanceled && price.BillingIntervalHours != nil && *price.BillingIntervalHours > 0:
		hours = int(*price.BillingIntervalHours)
	default:
		return History{}, nil
	}
	if !receipt.Approved {
		return History{Skip: true}, nil
	}
	currency := moneyutil.NormalizeCurrency(receipt.Currency)
	if receipt.Amount <= 0 || currency != price.Currency {
		return History{}, nil
	}
	rail := string(sub.Rail)
	if existing, err := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: sub.MerchantID, PspID: &sub.PspID,
		Channel: string(models.ChannelRail), Rail: &rail, TransactionID: receipt.TransactionID}); err == nil {
		if existing.CustomerID != sub.CustomerID || existing.Amount != receipt.Amount || existing.Currency != currency || existing.SubscriptionID != nil && *existing.SubscriptionID != sub.ID {
			return History{}, ErrConflict
		}
		return History{Skip: true}, nil
	} else if !db.IsNotFound(err) {
		return History{}, err
	}
	description := "Subscription"
	if product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: sub.MerchantID, ID: price.ProductID}); err == nil && strings.TrimSpace(product.DisplayName) != "" {
		description = product.DisplayName
	} else if err != nil && !db.IsNotFound(err) {
		return History{}, err
	}
	kind := "renewal"
	if at.Truncate(24 * time.Hour).Equal(sub.StartedAt.UTC().Truncate(24 * time.Hour)) {
		kind = "initial"
	}
	seats := int64(1)
	if sub.Quantity != nil && *sub.Quantity > 0 {
		seats = int64(*sub.Quantity)
	}
	var interval *int
	if hours > 0 {
		interval = &hours
	}
	bill := Bill{PSPID: sub.PspID, CustomerID: sub.CustomerID, SubscriptionID: &sub.ID, Owner: string(sub.CollectionPolicy), Kind: kind,
		SourceKind: SourceHistory, SourceKey: receipt.TransactionID, ProviderReference: sub.RailSubscriptionID, Currency: currency, Total: receipt.Amount,
		PeriodStart: at, PeriodEnd: scheduleBoundary(at, hours, months, 1), DueAt: at,
		Lines: []Line{{PriceID: &price.ID, ProductID: &price.ProductID, Description: description, Quantity: sub.Quantity, UnitAmount: receipt.Amount / seats,
			Amount: receipt.Amount, BillingIntervalHours: interval, AccessDurationHours: sub.AccessDurationHoursSnapshot}}}
	return History{Bill: &bill}, nil
}
