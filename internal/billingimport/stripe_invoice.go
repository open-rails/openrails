package billingimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// declaredStripeBill preserves explicitly supplied financial evidence. It
// never fetches a provider inside the import transaction or synthesizes a bill.
func declaredStripeBill(book DeclaredBilling, txn DeclaredTransaction, snapshots map[string][]byte) (*subscriptions.StripeBill, error) {
	invoice := txn.StripeInvoice
	if invoice == nil {
		return nil, nil
	}
	if invoice.ID == "" || len(invoice.ID) > 512 || invoice.CustomerReference == "" || len(invoice.Lines) == 0 || len(invoice.Lines) > 1000 || len(invoice.Payments) > 1000 {
		return nil, errors.New("complete invoice identity and bounded lines/payments are required")
	}
	if invoice.Status != "open" && invoice.Status != "paid" && invoice.Status != "uncollectible" && invoice.Status != "void" {
		return nil, errors.New("invoice must be finalized")
	}
	var found bool
	for _, sub := range book.Subscriptions {
		if sub.RailSubscriptionID != txn.RailSubscriptionID {
			continue
		}
		if sub.Rail != "stripe" || sub.PaymentMethod == nil || sub.PaymentMethod.RailCustomerRef != invoice.CustomerReference {
			return nil, errors.New("invoice customer must match the declared Stripe customer binding")
		}
		found = true
	}
	if !found {
		return nil, errors.New("invoice has no declared subscription")
	}
	raw, err := json.Marshal(invoice)
	if err != nil {
		return nil, err
	}
	key := txn.RailSubscriptionID + "\x00" + invoice.ID
	if prior, ok := snapshots[key]; ok && !bytes.Equal(prior, raw) {
		return nil, errors.New("receipts disagree on the complete invoice snapshot")
	}
	snapshots[key] = raw
	minor := func(amount int64) (int64, error) {
		n, err := moneyutil.NativeToRailMinorExact(invoice.Currency, amount)
		return int64(n), err
	}
	bill := &subscriptions.StripeBill{ID: invoice.ID, SubscriptionID: txn.RailSubscriptionID, CustomerID: invoice.CustomerReference, Status: invoice.Status, Reason: invoice.Reason, Currency: moneyutil.NormalizeCurrency(invoice.Currency), DueAt: invoice.DueAt}
	pairs := []struct {
		input  int64
		output *int64
	}{{invoice.Total, &bill.Total}, {invoice.AmountDue, &bill.AmountDue}, {invoice.AmountPaid, &bill.AmountPaid}, {invoice.AmountRemaining, &bill.AmountRemaining}, {invoice.AmountOffStripe, &bill.AmountOffStripe}, {invoice.StartingBalance, &bill.StartingBalance}, {invoice.PrePaymentCredit, &bill.PrePaymentCredit}}
	for _, p := range pairs {
		v, err := minor(p.input)
		if err != nil {
			return nil, err
		}
		*p.output = v
	}
	ids := map[string]bool{}
	for _, line := range invoice.Lines {
		if line.ID == "" || ids[line.ID] || len(line.ID) > 512 || len(line.Description) > 4096 || line.Quantity != nil && *line.Quantity <= 0 {
			return nil, errors.New("invoice line identity or quantity is invalid")
		}
		ids[line.ID] = true
		amount, err := minor(line.Amount)
		if err != nil {
			return nil, err
		}
		bill.Lines = append(bill.Lines, subscriptions.StripeBillLine{ID: line.ID, PriceID: line.PriceReference, Description: line.Description, Quantity: line.Quantity, Amount: amount, PeriodStart: line.PeriodStart, PeriodEnd: line.PeriodEnd})
	}
	found = false
	ids = map[string]bool{}
	for _, payment := range invoice.Payments {
		if payment.ID == "" || payment.ChargeID == "" || len(payment.ID) > 512 || len(payment.ChargeID) > 512 || ids[payment.ID] || payment.Amount <= 0 || payment.PaidAt.IsZero() {
			return nil, errors.New("invoice cash requires distinct allocation and Charge identities, exact amount and payment time")
		}
		ids[payment.ID] = true
		amount, err := minor(payment.Amount)
		if err != nil {
			return nil, err
		}
		bill.Payments = append(bill.Payments, subscriptions.StripeBillPayment{ID: payment.ID, ChargeID: payment.ChargeID, PaymentIntentID: payment.PaymentIntentID, Status: "paid", Kind: "charge", Currency: bill.Currency, Amount: amount, PaidAt: payment.PaidAt})
		if payment.ChargeID == txn.TransactionID || payment.PaymentIntentID != "" && payment.PaymentIntentID == txn.TransactionID {
			if !txn.Success || txn.Amount != payment.Amount || !strings.EqualFold(txn.Currency, bill.Currency) || !txn.OccurredAt.Equal(payment.PaidAt) {
				return nil, errors.New("transaction differs from its invoice monetary allocation")
			}
			found = true
		}
	}
	if txn.Success && !found {
		return nil, fmt.Errorf("transaction %s is not an actual cash allocation on invoice %s", txn.TransactionID, invoice.ID)
	}
	return bill, nil
}
