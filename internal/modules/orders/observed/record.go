package observed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/orderevent"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Record writes verified bill and payment facts in the caller's transaction.
// It never charges a provider or changes subscription access. The caller must
// retain rejected observations for reconciliation rather than acknowledge loss.
func Record(ctx context.Context, d *db.DB, bill Bill, now time.Time) (Result, error) {
	var result Result
	if d == nil {
		return result, errors.New("observed bill requires a transaction")
	}
	if _, ok := d.Qx(ctx).(pgx.Tx); !ok {
		return result, errors.New("observed bill requires a transaction-bound database")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return result, err
	}
	if err := validate(bill); err != nil {
		return result, err
	}
	now = now.UTC()
	q := d.Gen(ctx)
	if err := q.LockProviderBillIdentity(ctx, gen.LockProviderBillIdentityParams{MerchantID: mid.String(), PspID: bill.PSPID.String(), SourceKind: bill.SourceKind, SourceKey: bill.SourceKey}); err != nil {
		return result, err
	}
	snapshot, err := snapshotOf(bill)
	if err != nil {
		return result, err
	}
	identity := gen.GetProviderBillParams{MerchantID: mid.UUID(), PspID: bill.PSPID, SourceKind: bill.SourceKind, SourceKey: bill.SourceKey}
	accepted, err := q.GetProviderBill(ctx, identity)
	switch {
	case err == nil:
		if !sameJSON(accepted.Snapshot, snapshot) || accepted.CustomerID != bill.CustomerID || (bill.ExistingOrderID != nil && *bill.ExistingOrderID != accepted.OrderID) {
			return result, ErrConflict
		}
		result.Order, err = q.LockOrder(ctx, gen.LockOrderParams{MerchantID: mid.UUID(), ID: accepted.OrderID})
		if err != nil {
			return result, err
		}
	case errors.Is(err, pgx.ErrNoRows):
		result.Order, result.Created, err = admit(ctx, q, mid.UUID(), bill, now)
		if err != nil {
			return result, err
		}
		err = q.CreateProviderBill(ctx, gen.CreateProviderBillParams{MerchantID: mid.UUID(), PspID: bill.PSPID, SourceKind: bill.SourceKind, SourceKey: bill.SourceKey, OrderID: result.Order.ID, CustomerID: bill.CustomerID, Snapshot: snapshot, ProviderReference: bill.ProviderReference, Now: now})
		if err != nil {
			return result, err
		}
	default:
		return result, err
	}
	if result.Order.CustomerID != bill.CustomerID || result.Order.Currency != bill.Currency || result.Order.Total != bill.Total || result.Order.CollectionOwner != bill.Owner || result.Order.PspID == nil || *result.Order.PspID != bill.PSPID {
		return result, ErrConflict
	}
	if bill.Payment != nil {
		payment := *bill.Payment
		payment.OrderID = &result.Order.ID
		payment.PspID = &bill.PSPID
		if payment.ID == uuid.Nil {
			payment.ID = uuidutil.NewV7()
		}
		if payment.CreatedAt.IsZero() {
			payment.CreatedAt = now
		}
		if payment.PurchasedAt.IsZero() {
			payment.PurchasedAt = now
		}
		svc := payments.NewPaymentService(d)
		result.PaymentCreated, err = svc.CreateIfNotExists(db.WithPSPID(ctx, bill.PSPID), &payment)
		if err != nil {
			return result, fmt.Errorf("record observed payment: %w", err)
		}
		if !result.PaymentCreated {
			stored, err := svc.GetByPSPTransactionID(db.WithPSPID(ctx, bill.PSPID), payment.Rail, payment.TransactionID)
			if err != nil {
				return result, err
			}
			if !samePayment(stored, &payment) {
				return result, ErrConflict
			}
			payment = *stored
		}
		result.Payment = &payment
	}
	captured, err := q.ObservedOrderCaptured(ctx, gen.ObservedOrderCapturedParams{MerchantID: mid.UUID(), OrderID: result.Order.ID})
	if err != nil {
		return result, err
	}
	noncash := bill.NonCashAmount
	if accepted.SettledAt != nil {
		noncash = accepted.NonCashAmount
		if bill.Settled {
			if bill.NonCashAmount != accepted.NonCashAmount {
				evidence, _ := json.Marshal(map[string]any{"order_id": billing.OrderID(result.Order.ID).String(), "currency": bill.Currency,
					"accepted_non_cash_amount": strconv.FormatInt(accepted.NonCashAmount, 10), "observed_non_cash_amount": strconv.FormatInt(bill.NonCashAmount, 10)})
				action := "Review changed provider settlement evidence; the original settlement and every verified cash movement were retained."
				if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid.UUID(), PspID: &bill.PSPID, FindingType: "pull.bill.settlement_conflict", Source: pullSource(), SubjectKey: "order:" + result.Order.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
					return result, err
				}
			}
			// A later receipt cannot rewrite how this obligation was first
			// settled, nor roll back new verified cash merely because its
			// provider now reports different non-cash evidence.
			bill.NonCashAmount, bill.SettlementSource = accepted.NonCashAmount, models.DerefStr(accepted.SettlementSource)
		}
	}
	if captured > bill.Total || noncash > bill.Total-captured {
		evidence, _ := json.Marshal(map[string]any{"order_id": billing.OrderID(result.Order.ID).String(), "currency": bill.Currency,
			"billed_amount": strconv.FormatInt(bill.Total, 10), "captured_amount": strconv.FormatInt(captured, 10), "non_cash_amount": strconv.FormatInt(noncash, 10)})
		action := "Review the distinct captured payments and refund any excess; do not collect this order again."
		if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid.UUID(), PspID: &bill.PSPID, FindingType: "pull.bill.overpaid", Source: pullSource(), SubjectKey: "order:" + result.Order.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
			return result, err
		}
	}
	if result.PaymentCreated && bill.SubscriptionID != nil {
		sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: mid.UUID(), ID: *bill.SubscriptionID})
		if err != nil {
			return result, err
		}
		cancel := models.DerefStr(sub.CancelType)
		if sub.Status == "canceled" && (bill.Owner == OwnerEngine || cancel == "user" || cancel == "merchant" || cancel == "chargeback") {
			evidence, _ := json.Marshal(map[string]any{"order_id": billing.OrderID(result.Order.ID).String(), "payment_id": billing.PaymentID(result.Payment.ID).String(), "subscription_id": billing.SubscriptionID(*bill.SubscriptionID).String()})
			action := "Review this confirmed charge after cancellation and refund it if appropriate; billing and access remain canceled."
			if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: mid.UUID(), PspID: &bill.PSPID, FindingType: "pull.charge.after_cancellation", Source: pullSource(), SubjectKey: "payment:" + result.Payment.ID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
				return result, err
			}
		}
	}
	if !bill.Settled {
		return result, nil
	}
	if captured < bill.Total && bill.NonCashAmount < bill.Total-captured {
		return result, errors.New("settled provider bill lacks verified cash or non-cash settlement")
	}
	if _, err := q.SetProviderBillSettled(ctx, gen.SetProviderBillSettledParams{MerchantID: mid.UUID(), PspID: bill.PSPID, SourceKind: bill.SourceKind, SourceKey: bill.SourceKey, SettlementSource: bill.SettlementSource, NonCashAmount: bill.NonCashAmount, Now: now}); err != nil {
		return result, err
	}
	// A late receipt retains a closed order's disposition. It never revives an
	// expired/canceled purchase or a voided subscription obligation.
	if result.Order.Status != "open" && result.Order.Status != "processing" {
		return result, nil
	}
	var paymentID *uuid.UUID
	if result.Payment != nil {
		paymentID = &result.Payment.ID
	}
	if paymentID == nil {
		paymentID = result.Order.PaymentID
	}
	number, err := q.NextDocumentNumber(ctx, gen.NextDocumentNumberParams{MerchantID: mid.UUID(), Now: now})
	if err != nil {
		return result, err
	}
	n, err := q.SetOrderComplete(ctx, gen.SetOrderCompleteParams{MerchantID: mid.UUID(), ID: result.Order.ID, Number: orderevent.Number(number), PaymentID: paymentID, Now: now})
	if err != nil {
		return result, err
	}
	result.Completed = n == 1
	if result.Completed {
		result.Order.Status, result.Order.PaymentStatus = "complete", "succeeded"
		result.Order.PaymentID = paymentID
		if err := orderevent.Enqueue(ctx, q, &result.Order, "order.completed", now, map[string]any{"number": orderevent.Number(number), "settlement_source": bill.SettlementSource}); err != nil {
			return result, err
		}
	}
	result.Order, err = q.GetOrder(ctx, gen.GetOrderParams{MerchantID: mid.UUID(), ID: result.Order.ID})
	return result, err
}

// pullSource names a provider read as a pull.* finding's raiser.
func pullSource() *string {
	s := string(billing.ProposalSourcePull)
	return &s
}

func validate(b Bill) error {
	if b.PSPID == uuid.Nil || b.CustomerID == uuid.Nil || strings.TrimSpace(b.SourceKind) == "" || len(b.SourceKind) > 128 || strings.TrimSpace(b.SourceKey) == "" || len(b.SourceKey) > 512 || len(b.ProviderReference) > 512 || b.Total < 0 || b.NonCashAmount < 0 {
		return errors.New("provider bill identity or amount is invalid")
	}
	if b.Owner != OwnerEngine && b.Owner != OwnerProvider && b.Owner != OwnerNMISchedule {
		return errors.New("provider bill collection owner is invalid")
	}
	if err := moneyutil.RequireFiatCurrency(b.Currency); err != nil {
		return err
	}
	if b.Currency != moneyutil.NormalizeCurrency(b.Currency) {
		return errors.New("provider bill currency must be canonical")
	}
	switch b.Kind {
	case "purchase":
		if b.SubscriptionID != nil {
			return errors.New("purchase bill cannot name a subscription")
		}
	case "initial", "renewal", "adjustment":
		if b.SubscriptionID == nil || *b.SubscriptionID == uuid.Nil || b.PeriodStart.IsZero() || !b.PeriodEnd.After(b.PeriodStart) {
			return errors.New("subscription bill requires verified subscription and service period")
		}
	default:
		return errors.New("provider bill kind is invalid")
	}
	if len(b.Lines) == 0 || len(b.Lines) > 100 {
		return errors.New("provider bill requires between one and one hundred frozen lines")
	}
	var sum int64
	for _, l := range b.Lines {
		if strings.TrimSpace(l.Description) == "" || l.Quantity != nil && (*l.Quantity < 1 || *l.Quantity > math.MaxInt32) || l.UnitAmount < 0 || l.Amount < 0 && l.Source != "provider_adjustment" {
			return errors.New("invalid provider bill line")
		}
		if l.Source == "" && (l.PriceID == nil || l.ProductID == nil) {
			return errors.New("catalog bill line requires price and product")
		}
		if l.Source != "" && (l.PriceID != nil || l.ProductID != nil || l.Ownership != "" || l.ClaimKey != "") {
			return errors.New("provider adjustment must not pretend to be a catalog line")
		}
		if l.Amount > 0 && sum > math.MaxInt64-l.Amount || l.Amount < 0 && sum < math.MinInt64-l.Amount {
			return errors.New("provider bill total overflow")
		}
		sum += l.Amount
	}
	if sum != b.Total {
		return errors.New("provider bill lines do not match its frozen total")
	}
	if b.Settled {
		switch b.SettlementSource {
		case "cash":
			if b.NonCashAmount != 0 {
				return errors.New("cash settlement contains non-cash amount")
			}
		case "credit", "out_of_band", "mixed":
		default:
			return errors.New("verified settlement source is required")
		}
	} else if b.SettlementSource != "" || b.NonCashAmount != 0 {
		return errors.New("unsettled bill contains settlement facts")
	}
	if p := b.Payment; p != nil {
		if p.CustomerID != b.CustomerID || p.PspID != nil && *p.PspID != b.PSPID || p.Currency != b.Currency || p.Amount <= 0 || p.Status != "succeeded" && p.Status != "refunded" || p.RefundedPaymentID != nil || p.MoneyMovement != models.MoneyMovementRail || p.TransactionID == "" || !sameID(p.SubscriptionID, b.SubscriptionID) {
			return errors.New("bill payment must be an actual scoped positive capture")
		}
	}
	return nil
}

func snapshotOf(b Bill) ([]byte, error) {
	b.ExistingOrderID, b.Payment = nil, nil
	b.Settled, b.SettlementSource, b.NonCashAmount = false, "", 0
	b.PeriodStart, b.PeriodEnd, b.DueAt = b.PeriodStart.UTC(), b.PeriodEnd.UTC(), b.DueAt.UTC()
	return json.Marshal(b)
}
func sameJSON(a, b []byte) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	ca, _ := json.Marshal(x)
	cb, _ := json.Marshal(y)
	return bytes.Equal(ca, cb)
}
func sameID(a, b *uuid.UUID) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func samePayment(a, b *models.Payment) bool {
	return sameID(a.OrderID, b.OrderID) && sameID(a.SubscriptionID, b.SubscriptionID) && sameID(a.PspID, b.PspID) && a.CustomerID == b.CustomerID && a.Amount == b.Amount && a.ListAmount == b.ListAmount && sameGrant(a.CreditGrantSnapshot, b.CreditGrantSnapshot) && a.Currency == b.Currency && a.Rail == b.Rail && a.TransactionID == b.TransactionID && a.RefundedPaymentID == nil && a.MoneyMovement == b.MoneyMovement && (a.Status == "succeeded" || a.Status == "refunded")
}

func sameGrant(a, b *models.CreditGrantSnapshot) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
