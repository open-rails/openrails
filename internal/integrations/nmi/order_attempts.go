package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
)

// OrderAttempts is what the Query API holds under one order reference: every
// transaction of any outcome, not only approved sales.
type OrderAttempts struct {
	Transactions int
	// Declined is set when the order's only transaction is one refused sale
	// with a definite response code.
	Declined             bool
	DeclineCode          int
	DeclineTransactionID string
}

// ReadOrderAttempts reads the order with the account's own key. An error is an
// inconclusive read; zero transactions is the gateway's answer that nothing was
// recorded under the order.
func (c *NMIClient) ReadOrderAttempts(ctx context.Context, orderReference string) (OrderAttempts, error) {
	if c.accountSecurityKey != "" {
		scoped := *c
		scoped.SecurityKey = c.accountSecurityKey
		c = &scoped
	}
	if strings.TrimSpace(orderReference) == "" {
		return OrderAttempts{}, errors.New("order reference is required")
	}
	raw, err := c.SearchTransactions(ctx, QueryFilter{OrderID: orderReference})
	if err != nil {
		return OrderAttempts{}, err
	}
	var query saleQueryResponse
	if err := xml.Unmarshal([]byte(raw), &query); err != nil {
		return OrderAttempts{}, err
	}
	if query.ErrorResponse != "" {
		return OrderAttempts{}, errors.New(query.ErrorResponse)
	}
	var out OrderAttempts
	for _, txn := range query.Transactions {
		if txn.OrderID != orderReference {
			return OrderAttempts{}, receiptMismatch("order search returned another order's transaction")
		}
		out.Transactions++
	}
	if out.Transactions != 1 {
		return out, nil
	}
	txn := query.Transactions[0]
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
