package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrReceiptMismatch reports that an exact provider object exists but does not
// identify the expected operation, or does not exist at all.
var ErrReceiptMismatch = errors.New("nmi receipt does not match the operation")

func receiptMismatch(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrReceiptMismatch, fmt.Sprintf(format, args...))
}

// exactMinorAmount parses a provider major-unit decimal at its declared
// currency scale, without rounding or float conversion (JPY has no decimals).
func exactMinorAmount(amount, currency string) (int64, bool) {
	units, ok := moneyutil.LookupCurrency(currency)
	if !ok {
		return 0, false
	}
	amount = strings.TrimSpace(amount)
	if amount == "" {
		return 0, false
	}
	for i, ch := range amount {
		if (ch < '0' || ch > '9') && ch != '.' && !(i == 0 && ch == '-') {
			return 0, false
		}
	}
	value, ok := new(big.Rat).SetString(amount)
	if !ok {
		return 0, false
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(units.MinorDecimals)), nil)
	value.Mul(value, new(big.Rat).SetInt(scale))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, false
	}
	return value.Num().Int64(), true
}

func successfulAction(txn v5Transaction, actionType string, amount moneyutil.Cents) bool {
	for _, action := range txn.Actions {
		cents, ok := exactMinorAmount(action.Amount, txn.Currency)
		if cents < 0 {
			cents = -cents
		}
		if strings.EqualFold(strings.TrimSpace(action.Type), actionType) && action.Success && ok && cents == int64(amount) {
			return true
		}
	}
	return false
}

// ConfirmApprovedSale reads one transaction by its exact id and requires an
// approved sale of exactly amount, in currency, on the customer vault. It
// does not establish which operation the sale belongs to: callers bind the
// transaction to their order reference separately.
func (c *NMIClient) ConfirmApprovedSale(ctx context.Context, transactionID, customerVaultID string, amount moneyutil.Cents, currency string) error {
	txn, err := c.approvedTransaction(ctx, transactionID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(customerVaultID) == "" || strings.TrimSpace(txn.CustomerVaultID) != strings.TrimSpace(customerVaultID) {
		return receiptMismatch("transaction %s is not on the operation's customer vault", transactionID)
	}
	return saleMatches(txn, transactionID, amount, currency)
}

// ConfirmApprovedUnvaultedSale is the exact read for a sale charged by card
// data through a custodian proxy: such a sale has no customer vault at NMI,
// so approval, currency and amount are all the read can bind. The order
// reference binds the operation; callers check it separately.
func (c *NMIClient) ConfirmApprovedUnvaultedSale(ctx context.Context, transactionID string, amount moneyutil.Cents, currency string) error {
	txn, err := c.approvedTransaction(ctx, transactionID)
	if err != nil {
		return err
	}
	return saleMatches(txn, transactionID, amount, currency)
}

func (c *NMIClient) approvedTransaction(ctx context.Context, transactionID string) (v5Transaction, error) {
	txn, found, err := c.GetPayment(ctx, transactionID)
	if err != nil {
		return txn, err
	}
	switch {
	case !found:
		return txn, receiptMismatch("transaction %s does not exist", transactionID)
	case !txn.approved():
		return txn, receiptMismatch("transaction %s is not approved", transactionID)
	}
	return txn, nil
}

func saleMatches(txn v5Transaction, transactionID string, amount moneyutil.Cents, currency string) error {
	switch {
	case strings.TrimSpace(currency) == "" || !strings.EqualFold(strings.TrimSpace(txn.Currency), strings.TrimSpace(currency)):
		return receiptMismatch("transaction %s is not in %s", transactionID, currency)
	case !successfulAction(txn, "sale", amount):
		return receiptMismatch("transaction %s has no successful sale of %d cents", transactionID, amount)
	}
	return nil
}

// ConfirmLiveSubscription reads one subscription by its exact id and requires
// a live record on the vault and plan.
func (c *NMIClient) ConfirmLiveSubscription(ctx context.Context, subscriptionID, customerVaultID, planID string) error {
	sub, found, err := c.GetSubscription(ctx, subscriptionID)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return receiptMismatch("subscription %s is not live", subscriptionID)
	case strings.TrimSpace(customerVaultID) == "" || strings.TrimSpace(sub.CustomerVaultID) != strings.TrimSpace(customerVaultID):
		return receiptMismatch("subscription %s is not on the operation's customer vault", subscriptionID)
	case sub.Plan == nil || strings.TrimSpace(planID) == "" || strings.TrimSpace(sub.Plan.ID) != strings.TrimSpace(planID):
		return receiptMismatch("subscription %s is not on the operation's plan", subscriptionID)
	}
	return nil
}

// ConfirmRefund reads the refund transaction by its exact id and requires an
// approved refund of exactly amount and currency on the original transaction's vault. A
// zero amount means a full refund of the original sale.
func (c *NMIClient) ConfirmRefund(ctx context.Context, originalTransactionID, refundTransactionID string, amount moneyutil.Cents, currency string) error {
	original, found, err := c.GetPayment(ctx, originalTransactionID)
	if err != nil {
		return err
	}
	if !found || original.ID != originalTransactionID {
		return receiptMismatch("original transaction %s does not exist", originalTransactionID)
	}
	if _, ok := moneyutil.LookupCurrency(currency); !ok || currency == "" || !strings.EqualFold(strings.TrimSpace(original.Currency), currency) {
		return receiptMismatch("original transaction %s is not in %s", originalTransactionID, currency)
	}
	if amount == 0 {
		cents, ok := exactMinorAmount(original.Amount, currency)
		if !ok || cents <= 0 {
			return receiptMismatch("original transaction %s has no exact amount", originalTransactionID)
		}
		amount = moneyutil.Cents(cents)
	}
	refund, found, err := c.GetPayment(ctx, refundTransactionID)
	if err != nil {
		return err
	}
	switch {
	case !found || refund.ID != refundTransactionID:
		return receiptMismatch("refund %s does not exist", refundTransactionID)
	case !strings.EqualFold(strings.TrimSpace(refund.Currency), currency):
		return receiptMismatch("refund %s is not in %s", refundTransactionID, currency)
	case !refund.approved():
		return receiptMismatch("refund %s is not approved", refundTransactionID)
	case strings.TrimSpace(original.CustomerVaultID) == "" || strings.TrimSpace(refund.CustomerVaultID) != strings.TrimSpace(original.CustomerVaultID):
		return receiptMismatch("refund %s is not on the original transaction's customer vault", refundTransactionID)
	case !successfulAction(refund, "refund", amount):
		return receiptMismatch("refund %s has no successful refund of %d minor units", refundTransactionID, amount)
	}
	return nil
}

// SaleEvidence contains only facts read from the authenticated provider account.
// The order search and exact transaction must identify the same successful sale.
// No card data or arbitrary provider response is retained.
type SaleEvidence struct {
	// VaultBillingID is the sole billing entry from an authenticated vault read.
	// It is not a field echoed by the transaction response.
	VaultBillingID  string          `json:"vault_billing_id,omitempty"`
	TransactionID   string          `json:"transaction_id"`
	OrderReference  string          `json:"order_reference"`
	CustomerVaultID string          `json:"customer_vault_id"`
	Amount          moneyutil.Cents `json:"amount,string"`
	Currency        string          `json:"currency"`
	Approved        bool            `json:"approved"`
}

func (c *NMIClient) ReadSaleEvidence(ctx context.Context, orderReference, reference string) (SaleEvidence, bool, error) {
	if c.accountSecurityKey != "" {
		scoped := *c
		scoped.SecurityKey = c.accountSecurityKey
		c = &scoped
	}

	if strings.TrimSpace(orderReference) == "" {
		return SaleEvidence{}, false, errors.New("order reference is required")
	}
	raw, err := c.SearchTransactions(ctx, QueryFilter{OrderID: orderReference})
	if err != nil {
		return SaleEvidence{}, false, err
	}
	var query saleQueryResponse
	if err := xml.Unmarshal([]byte(raw), &query); err != nil {
		return SaleEvidence{}, false, err
	}
	if query.ErrorResponse != "" {
		return SaleEvidence{}, false, errors.New(query.ErrorResponse)
	}
	id := ""
	for _, txn := range query.Transactions {
		for _, action := range txn.Actions {
			if !strings.EqualFold(strings.TrimSpace(action.ActionType), "sale") || strings.TrimSpace(action.Success) != "1" {
				continue
			}
			if txn.OrderID != orderReference || strings.TrimSpace(txn.TransactionID) == "" {
				return SaleEvidence{}, false, receiptMismatch("order search returned an unbound successful sale")
			}
			if id != "" && id != txn.TransactionID {
				return SaleEvidence{}, false, receiptMismatch("order has multiple successful sales")
			}
			id = txn.TransactionID
		}
	}
	if id == "" {
		return SaleEvidence{}, false, nil
	}
	if reference != "" && id != reference {
		return SaleEvidence{}, false, receiptMismatch("named transaction does not match order's successful sale")
	}
	txn, err := c.approvedTransaction(ctx, id)
	if err != nil {
		return SaleEvidence{}, false, err
	}
	if txn.ID != id {
		return SaleEvidence{}, false, receiptMismatch("exact transaction read returned a different identity")
	}
	var amount moneyutil.Cents
	for _, action := range txn.Actions {
		if !strings.EqualFold(strings.TrimSpace(action.Type), "sale") || !action.Success {
			continue
		}
		cents, ok := exactMinorAmount(action.Amount, txn.Currency)
		if !ok || cents <= 0 || amount != 0 {
			return SaleEvidence{}, false, receiptMismatch("transaction does not have one exact positive sale")
		}
		amount = moneyutil.Cents(cents)
	}
	if amount <= 0 || strings.TrimSpace(txn.Currency) == "" {
		return SaleEvidence{}, false, receiptMismatch("sale evidence is incomplete")
	}
	return SaleEvidence{TransactionID: id, OrderReference: orderReference, CustomerVaultID: strings.TrimSpace(txn.CustomerVaultID), Amount: amount, Currency: strings.ToUpper(strings.TrimSpace(txn.Currency)), Approved: true}, true, nil
}
