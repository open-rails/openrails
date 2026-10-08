package nmi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrNotInvoiceReceipt distinguishes ordinary provider transactions from the
// exact invoice description emitted by OpenRails. Malformed invoice claims fail.
var ErrNotInvoiceReceipt = errors.New("NMI transaction does not identify an invoice")

// InvoiceSaleEvidence is read from the authenticated account, never a request's
// proposed amount or payer. The MoneyService seals it before local recovery.
type InvoiceSaleEvidence struct {
	InvoiceID uuid.UUID
	Sale      SaleEvidence
	PaidAt    time.Time
}

// ReadInvoiceSaleEvidence qualifies one observed full invoice sale. Description
// identifies the retained invoice; order still identifies the original operation.
// All matching history is read so another successful sale or reversal refuses
// automatic allocation. No financial request is made.
func (c *NMIClient) ReadInvoiceSaleEvidence(ctx context.Context, transactionID string) (InvoiceSaleEvidence, error) {
	c = c.scoped()
	if transactionID == "" || strings.TrimSpace(transactionID) != transactionID || len(transactionID) > 255 {
		return InvoiceSaleEvidence{}, errors.New("invalid invoice transaction identity")
	}
	report, err := c.TransactionReport(ctx, QueryFilter{TransactionID: transactionID})
	if err != nil {
		return InvoiceSaleEvidence{}, err
	}
	if len(report.Transactions) != 1 || report.Transactions[0].TransactionID != transactionID {
		return InvoiceSaleEvidence{}, errors.New("invoice transaction lookup is not exact")
	}
	observed := report.Transactions[0]
	if !strings.HasPrefix(observed.OrderDescription, "invoice ") {
		return InvoiceSaleEvidence{}, ErrNotInvoiceReceipt
	}
	invoice, err := uuid.Parse(strings.TrimPrefix(observed.OrderDescription, "invoice "))
	if err != nil || invoice == uuid.Nil || observed.OrderDescription != "invoice "+invoice.String() {
		return InvoiceSaleEvidence{}, errors.New("invoice description is not an exact invoice identity")
	}
	order, err := uuid.Parse(observed.OrderID)
	if err != nil || order == uuid.Nil || observed.OrderID != order.String() {
		return InvoiceSaleEvidence{}, errors.New("invoice sale has no exact operation order")
	}
	sale, found, err := c.ReadSaleEvidence(ctx, observed.OrderID, transactionID)
	if err != nil {
		return InvoiceSaleEvidence{}, err
	}
	if !found || sale.CustomerVaultID == "" ||
		(observed.CustomerVaultID != "" && sale.CustomerVaultID != observed.CustomerVaultID) ||
		(observed.Currency != "" && !strings.EqualFold(sale.Currency, observed.Currency)) {
		return InvoiceSaleEvidence{}, errors.New("invoice sale does not match exact provider read")
	}
	var paidAt time.Time
	var reportedMinor moneyutil.Cents
	candidates := map[string]bool{}
	const pageSize = 1000
	complete := false
	for page := 0; page < 100; page++ {
		history, err := c.TransactionReport(ctx, QueryFilter{OrderDescription: observed.OrderDescription, ResultLimit: pageSize, PageNumber: page})
		if err != nil {
			return InvoiceSaleEvidence{}, err
		}
		for _, txn := range history.Transactions {
			if txn.OrderDescription != observed.OrderDescription || txn.TransactionID == "" {
				return InvoiceSaleEvidence{}, errors.New("invoice history contains an unbound transaction")
			}
			if txn.Reversed() {
				return InvoiceSaleEvidence{}, errors.New("invoice history contains a reversal")
			}
			for _, action := range txn.Actions {
				if !action.Is("sale") && !action.Is("refund") && !action.Is("credit") && !action.Is("void") {
					continue
				}
				if action.Success != "0" && action.Success != "1" {
					return InvoiceSaleEvidence{}, errors.New("invoice history has an unreadable outcome")
				}
				if !action.Succeeded() {
					continue
				}
				if !action.Is("sale") {
					return InvoiceSaleEvidence{}, errors.New("invoice history contains a reversal")
				}
				if candidates[txn.TransactionID] {
					return InvoiceSaleEvidence{}, errors.New("invoice history repeats a successful sale")
				}
				candidates[txn.TransactionID] = true
				if txn.TransactionID != transactionID {
					return InvoiceSaleEvidence{}, errors.New("invoice has another successful provider sale")
				}
				if txn.OrderID != observed.OrderID || (txn.CustomerVaultID != "" && txn.CustomerVaultID != sale.CustomerVaultID) || (txn.Currency != "" && !strings.EqualFold(txn.Currency, sale.Currency)) {
					return InvoiceSaleEvidence{}, errors.New("invoice transaction changed while being read")
				}
				amount, ok := exactMinorAmount(action.Amount, sale.Currency)
				if !ok || amount <= 0 {
					return InvoiceSaleEvidence{}, errors.New("invoice sale has no exact positive amount")
				}
				at, ok := action.At()
				if !ok {
					return InvoiceSaleEvidence{}, errors.New("invoice sale has no readable time")
				}
				paidAt = at
				reportedMinor = moneyutil.Cents(amount)
			}
		}
		if len(history.Transactions) < pageSize {
			complete = true
			break
		}
	}
	if !complete || len(candidates) != 1 || paidAt.IsZero() {
		return InvoiceSaleEvidence{}, errors.New("invoice history is incomplete or has no successful sale")
	}
	if sale.Amount != reportedMinor {
		return InvoiceSaleEvidence{}, errors.New("invoice amount changed while being read")
	}
	payment, found, err := c.GetPayment(ctx, transactionID)
	if err != nil {
		return InvoiceSaleEvidence{}, err
	}
	if !found || payment.ID != transactionID {
		return InvoiceSaleEvidence{}, errors.New("invoice payment disappeared during verification")
	}
	for _, action := range payment.Actions {
		if (action.Success || action.Response == "1" || action.ResponseCode == "100") && (strings.EqualFold(action.Type, "refund") || strings.EqualFold(action.Type, "credit") || strings.EqualFold(action.Type, "void")) {
			return InvoiceSaleEvidence{}, fmt.Errorf("invoice transaction %s has a reversal", transactionID)
		}
	}
	return InvoiceSaleEvidence{InvoiceID: invoice, Sale: sale, PaidAt: paidAt}, nil
}
