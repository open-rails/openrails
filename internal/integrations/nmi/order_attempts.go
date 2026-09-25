package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// OrderAttempts is what the Query API holds under one order reference since
// an attempt's fence: every transaction of any outcome, not only approved
// sales. An order is shared by every attempt of one obligation, so earlier
// attempts' transactions are left out, except an approved sale, which counts
// wherever it falls.
type OrderAttempts struct {
	Transactions int
	// Declined is set when the attempt's only transaction is one refused sale
	// with a definite response code.
	Declined             bool
	DeclineCode          int
	DeclineTransactionID string
}

// queryTransaction is one transaction of a Query API transaction report.
type queryTransaction struct {
	TransactionID   string `xml:"transaction_id"`
	OrderID         string `xml:"order_id"`
	CustomerVaultID string `xml:"customer_vault_id"`
	Currency        string `xml:"currency"`
	Actions         []struct {
		Amount       string `xml:"amount"`
		ActionType   string `xml:"action_type"`
		Success      string `xml:"success"`
		Date         string `xml:"date"`
		ResponseCode string `xml:"response_code"`
	} `xml:"action"`
}

type transactionReport struct {
	XMLName       xml.Name           `xml:"nm_response"`
	Transactions  []queryTransaction `xml:"transaction"`
	ErrorResponse string             `xml:"error_response"`
}

// at is the transaction's first action time; a transaction without one is
// unreadable.
func (t queryTransaction) at() (time.Time, error) {
	var first time.Time
	for _, action := range t.Actions {
		ts, err := time.ParseInLocation(queryAPITimeFormat, strings.TrimSpace(action.Date), time.UTC)
		if err != nil {
			return time.Time{}, fmt.Errorf("transaction %s has an unreadable date", t.TransactionID)
		}
		if first.IsZero() || ts.Before(first) {
			first = ts
		}
	}
	if first.IsZero() {
		return time.Time{}, fmt.Errorf("transaction %s has no dated action", t.TransactionID)
	}
	return first, nil
}

// approvedSale is the amount of the transaction's approved sale, or 0.
func (t queryTransaction) approvedSale() int64 {
	for _, action := range t.Actions {
		if strings.EqualFold(strings.TrimSpace(action.ActionType), "sale") && strings.TrimSpace(action.Success) == "1" {
			if cents, ok := exactMinorAmount(action.Amount, t.Currency); ok && cents > 0 {
				return cents
			}
			return -1
		}
	}
	return 0
}

func (c *NMIClient) scoped() *NMIClient {
	if c.accountSecurityKey == "" {
		return c
	}
	scoped := *c
	scoped.SecurityKey = c.accountSecurityKey
	return &scoped
}

func (c *NMIClient) transactionReport(ctx context.Context, filter QueryFilter) (transactionReport, error) {
	raw, err := c.SearchTransactions(ctx, filter)
	if err != nil {
		return transactionReport{}, err
	}
	var report transactionReport
	if err := xml.Unmarshal([]byte(raw), &report); err != nil {
		return transactionReport{}, err
	}
	if report.ErrorResponse != "" {
		return transactionReport{}, errors.New(report.ErrorResponse)
	}
	return report, nil
}

// ReadOrderAttempts reads every transaction under an order owned by one
// operation. An error is an inconclusive read; zero transactions is the
// gateway's answer that nothing was recorded under the order.
func (c *NMIClient) ReadOrderAttempts(ctx context.Context, orderReference string) (OrderAttempts, error) {
	return c.ReadOrderAttemptsSince(ctx, orderReference, time.Time{})
}

// ReadOrderAttemptsSince reads a shared order for the attempt fenced at since.
func (c *NMIClient) ReadOrderAttemptsSince(ctx context.Context, orderReference string, since time.Time) (OrderAttempts, error) {
	c = c.scoped()
	if strings.TrimSpace(orderReference) == "" {
		return OrderAttempts{}, errors.New("order reference is required")
	}
	report, err := c.transactionReport(ctx, QueryFilter{OrderID: orderReference})
	if err != nil {
		return OrderAttempts{}, err
	}
	var mine []queryTransaction
	for _, txn := range report.Transactions {
		if txn.OrderID != orderReference {
			return OrderAttempts{}, receiptMismatch("order search returned another order's transaction")
		}
		if since.IsZero() || txn.approvedSale() != 0 {
			mine = append(mine, txn)
			continue
		}
		at, err := txn.at()
		if err != nil {
			return OrderAttempts{}, err
		}
		if !at.Before(since) {
			mine = append(mine, txn)
		}
	}
	out := OrderAttempts{Transactions: len(mine)}
	if len(mine) != 1 {
		return out, nil
	}
	txn := mine[0]
	for _, action := range txn.Actions {
		if !strings.EqualFold(strings.TrimSpace(action.ActionType), "sale") {
			continue
		}
		if strings.TrimSpace(action.Success) == "1" {
			return OrderAttempts{Transactions: 1}, nil
		}
		code, err := strconv.Atoi(strings.TrimSpace(action.ResponseCode))
		if err != nil || code <= 0 || UncertainResponseCode(code) || out.Declined {
			return OrderAttempts{Transactions: 1}, nil
		}
		out.Declined, out.DeclineCode, out.DeclineTransactionID = true, code, strings.TrimSpace(txn.TransactionID)
	}
	return out, nil
}

// VaultTransaction is one transaction on a customer vault, of any order and
// outcome.
type VaultTransaction struct {
	TransactionID string
	OrderID       string
	At            time.Time
	// ApprovedMinor is the approved sale's amount; 0 when the transaction is
	// not an approved sale.
	ApprovedMinor int64
}

const (
	vaultPageSize = 100
	vaultMaxPages = 20
)

// ReadVaultTransactions reads every transaction on the vault since since,
// paging through the Query API. A transaction naming no vault is kept: the
// read is evidence of absence only when nothing at all is returned.
func (c *NMIClient) ReadVaultTransactions(ctx context.Context, vaultID string, since time.Time) ([]VaultTransaction, error) {
	c = c.scoped()
	vaultID = strings.TrimSpace(vaultID)
	if vaultID == "" || since.IsZero() {
		return nil, errors.New("vault and start time are required")
	}
	var out []VaultTransaction
	for page := 0; page < vaultMaxPages; page++ {
		report, err := c.transactionReport(ctx, QueryFilter{CustomerVaultID: vaultID, StartDate: since.UTC().Format(queryAPITimeFormat), ResultLimit: vaultPageSize, PageNumber: page})
		if err != nil {
			return nil, err
		}
		for _, txn := range report.Transactions {
			if v := strings.TrimSpace(txn.CustomerVaultID); v != "" && v != vaultID {
				continue
			}
			at, err := txn.at()
			if err != nil {
				return nil, err
			}
			if at.Before(since) {
				continue
			}
			amount := txn.approvedSale()
			if amount < 0 {
				return nil, fmt.Errorf("transaction %s has an unreadable approved amount", txn.TransactionID)
			}
			out = append(out, VaultTransaction{TransactionID: strings.TrimSpace(txn.TransactionID), OrderID: strings.TrimSpace(txn.OrderID), At: at, ApprovedMinor: amount})
		}
		if len(report.Transactions) < vaultPageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("vault %s has more than %d transactions since %s", vaultID, vaultPageSize*vaultMaxPages, since.UTC().Format(time.RFC3339))
}
