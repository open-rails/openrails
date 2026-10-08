package nmi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// OrderAttempts describes the visible transactions for one accepted attempt.
// Any approved sale under the shared obligation order prevents a decline from
// releasing it. Renewal declines are bound by exact attempt description, not
// timestamps which can overlap when a payer retries immediately.
type OrderAttempts struct {
	Transactions int
	// Declined is set when the attempt's only transaction is one refused sale
	// with a definite response code.
	Declined             bool
	DeclineCode          int
	DeclineTransactionID string
}

// at is the transaction's first action time; a transaction without one is
// unreadable.
func (t QueryTransaction) at() (time.Time, error) {
	var first time.Time
	for _, action := range t.Actions {
		ts, ok := action.At()
		if !ok {
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
func (t QueryTransaction) approvedSale() int64 {
	for _, action := range t.Actions {
		if action.Is("sale") && action.Succeeded() {
			if cents, err := moneyutil.DecimalToRailMinor(t.Currency, action.Amount); err == nil && cents > 0 {
				return int64(cents)
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

// ReadOrderAttempts reads every transaction under an order owned by one
// operation. An error is an inconclusive read; zero transactions is the
// gateway's answer that nothing was recorded under the order.
func (c *NMIClient) ReadOrderAttempts(ctx context.Context, orderReference string) (OrderAttempts, error) {
	return c.readOrderAttempts(ctx, orderReference, nil)
}

// ReadRecurringOrderAttempts binds a declined sale to the exact transmitted
// attempt and financial terms. Its order remains shared by the whole period.
func (c *NMIClient) ReadRecurringOrderAttempts(ctx context.Context, accepted SaleParams) (OrderAttempts, error) {
	if accepted.OrderDescription == "" || accepted.CustomerVaultID == "" || accepted.BillingID == "" || accepted.Amount <= 0 || accepted.Currency == "" {
		return OrderAttempts{}, errors.New("recurring attempt requires exact description, instrument, amount and currency")
	}
	return c.readOrderAttempts(ctx, accepted.OrderID, &accepted)
}

func (c *NMIClient) readOrderAttempts(ctx context.Context, orderReference string, accepted *SaleParams) (OrderAttempts, error) {
	c = c.scoped()
	if strings.TrimSpace(orderReference) == "" {
		return OrderAttempts{}, errors.New("order reference is required")
	}
	report, err := c.TransactionReport(ctx, QueryFilter{OrderID: orderReference})
	if err != nil {
		return OrderAttempts{}, err
	}
	var mine []QueryTransaction
	for _, txn := range report.Transactions {
		if txn.OrderID != orderReference {
			return OrderAttempts{}, receiptMismatch("order search returned another order's transaction")
		}
		if accepted != nil && txn.approvedSale() == 0 {
			if txn.OrderDescription != accepted.OrderDescription {
				continue
			}
			if txn.CustomerVaultID != accepted.CustomerVaultID || !strings.EqualFold(txn.Currency, accepted.Currency) {
				return OrderAttempts{}, receiptMismatch("decline does not match accepted instrument and currency")
			}
		}
		mine = append(mine, txn)
	}
	out := OrderAttempts{Transactions: len(mine)}
	if len(mine) != 1 {
		return out, nil
	}
	txn := mine[0]
	if strings.TrimSpace(txn.TransactionID) == "" {
		return out, nil
	}
	for _, action := range txn.Actions {
		if !action.Is("sale") {
			continue
		}
		if action.Succeeded() {
			return OrderAttempts{Transactions: 1}, nil
		}
		if accepted != nil {
			amount, err := moneyutil.DecimalToRailMinor(txn.Currency, action.Amount)
			if err != nil || amount != accepted.Amount {
				return OrderAttempts{}, receiptMismatch("decline does not match accepted amount")
			}
			if _, err := c.ReadSingleCardVaultBilling(ctx, accepted.CustomerVaultID, accepted.BillingID); err != nil {
				return OrderAttempts{}, err
			}
		}
		code, err := strconv.Atoi(strings.TrimSpace(action.ResponseCode))
		if err != nil || code < 200 || code >= 300 || UncertainResponseCode(code) || out.Declined {
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
		report, err := c.TransactionReport(ctx, QueryFilter{CustomerVaultID: vaultID, StartDate: since.UTC().Format(QueryTimeFormat), ResultLimit: vaultPageSize, PageNumber: page})
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
