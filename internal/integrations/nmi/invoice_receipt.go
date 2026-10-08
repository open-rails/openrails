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
	facts, found, err := c.FindInvoiceSaleEvidence(ctx, invoice)
	if err != nil {
		return InvoiceSaleEvidence{}, err
	}
	if !found || facts.Sale.TransactionID != transactionID {
		return InvoiceSaleEvidence{}, errors.New("invoice transaction is not its sole successful sale")
	}
	if observed.OrderID != facts.Sale.OrderReference || (observed.CustomerVaultID != "" && observed.CustomerVaultID != facts.Sale.CustomerVaultID) || (observed.Currency != "" && !strings.EqualFold(observed.Currency, facts.Sale.Currency)) {
		return InvoiceSaleEvidence{}, errors.New("invoice transaction changed while being read")
	}
	return facts, nil
}

// FindInvoiceSaleEvidence reads the current invoice history without the bulk
// refresh's safety lag. An empty result is only a completed observation, never
// proof of nonexecution for a possibly submitted operation or an atomic claim
// against independently writable database copies.
func (c *NMIClient) FindInvoiceSaleEvidence(ctx context.Context, invoice uuid.UUID) (InvoiceSaleEvidence, bool, error) {
	c = c.scoped()
	if invoice == uuid.Nil {
		return InvoiceSaleEvidence{}, false, errors.New("invoice identity is required")
	}
	candidate, action, found, err := c.invoiceSaleCandidate(ctx, invoice)
	if err != nil || !found {
		return InvoiceSaleEvidence{}, found, err
	}
	order, err := uuid.Parse(candidate.OrderID)
	if err != nil || order == uuid.Nil || candidate.OrderID != order.String() {
		return InvoiceSaleEvidence{}, false, errors.New("invoice sale has no exact operation order")
	}
	sale, found, err := c.ReadSaleEvidence(ctx, candidate.OrderID, candidate.TransactionID)
	if err != nil {
		return InvoiceSaleEvidence{}, false, err
	}
	if !found || sale.CustomerVaultID == "" ||
		(candidate.CustomerVaultID != "" && sale.CustomerVaultID != candidate.CustomerVaultID) ||
		(candidate.Currency != "" && !strings.EqualFold(sale.Currency, candidate.Currency)) {
		return InvoiceSaleEvidence{}, false, errors.New("invoice sale does not match exact provider read")
	}
	amount, err := moneyutil.DecimalToRailMinor(sale.Currency, action.Amount)
	if err != nil || amount <= 0 || sale.Amount != amount {
		return InvoiceSaleEvidence{}, false, errors.New("invoice sale has no consistent exact positive amount")
	}
	paidAt, ok := action.At()
	if !ok {
		return InvoiceSaleEvidence{}, false, errors.New("invoice sale has no readable time")
	}
	payment, found, err := c.GetPayment(ctx, candidate.TransactionID)
	if err != nil {
		return InvoiceSaleEvidence{}, false, err
	}
	if !found || payment.ID != candidate.TransactionID {
		return InvoiceSaleEvidence{}, false, errors.New("invoice payment disappeared during verification")
	}
	for _, action := range payment.Actions {
		if (action.Success || action.Response == "1" || action.ResponseCode == "100") && (strings.EqualFold(action.Type, "refund") || strings.EqualFold(action.Type, "credit") || strings.EqualFold(action.Type, "void")) {
			return InvoiceSaleEvidence{}, false, fmt.Errorf("invoice transaction %s has a reversal", candidate.TransactionID)
		}
	}
	return InvoiceSaleEvidence{InvoiceID: invoice, Sale: sale, PaidAt: paidAt}, true, nil
}

func (c *NMIClient) invoiceSaleCandidate(ctx context.Context, invoice uuid.UUID) (candidate QueryTransaction, approved QueryAction, found bool, err error) {
	description := "invoice " + invoice.String()
	seen := map[string]bool{}
	const pageSize = 1000
	for page := 0; page < 100; page++ {
		history, err := c.TransactionReport(ctx, QueryFilter{OrderDescription: description, ResultLimit: pageSize, PageNumber: page})
		if err != nil {
			return candidate, approved, false, err
		}
		for _, txn := range history.Transactions {
			if txn.OrderDescription != description || txn.TransactionID == "" || seen[txn.TransactionID] {
				return candidate, approved, false, errors.New("invoice history contains an unbound or repeated transaction")
			}
			seen[txn.TransactionID] = true
			if txn.Reversed() {
				return candidate, approved, false, errors.New("invoice history contains a reversal")
			}
			hasSale := false
			for _, action := range txn.Actions {
				if !action.Is("sale") && !action.Is("refund") && !action.Is("credit") && !action.Is("void") && !action.Is("auth") {
					continue
				}
				if action.Success != "0" && action.Success != "1" {
					return candidate, approved, false, errors.New("invoice history has an unreadable outcome")
				}
				hasSale = hasSale || action.Is("sale")
				if !action.Succeeded() {
					if _, closed := action.DefinitiveDecline(txn.Condition); !closed {
						return candidate, approved, false, errors.New("invoice history contains an unresolved financial outcome")
					}
					continue
				}
				if !action.Is("sale") {
					return candidate, approved, false, errors.New("invoice history contains another financial action")
				}
				if found {
					return candidate, approved, false, errors.New("invoice has multiple successful provider sales")
				}
				candidate, approved, found = txn, action, true
			}
			if !hasSale {
				return candidate, approved, false, errors.New("invoice history does not establish a sale outcome")
			}
		}
		if len(history.Transactions) < pageSize {
			return candidate, approved, found, nil
		}
	}
	return candidate, approved, false, errors.New("invoice history exceeds the bounded page limit")
}
