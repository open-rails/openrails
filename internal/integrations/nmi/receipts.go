package nmi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrReceiptMismatch reports that an exact provider object exists but does not
// identify the expected operation, or does not exist at all.
var ErrReceiptMismatch = errors.New("nmi receipt does not match the operation")

func receiptMismatch(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrReceiptMismatch, fmt.Sprintf(format, args...))
}

// exactCents parses a v5 decimal amount, refusing sub-cent precision.
func exactCents(amount string) (int64, bool) {
	trimmed := strings.TrimSpace(amount)
	if _, frac, ok := strings.Cut(trimmed, "."); ok && len(frac) > 2 {
		return 0, false
	}
	cents, err := v5AmountToCents(trimmed)
	return cents, err == nil
}

func successfulAction(txn v5Transaction, actionType string, amount moneyutil.Cents) bool {
	for _, action := range txn.Actions {
		cents, ok := exactCents(action.Amount)
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
	txn, found, err := c.GetPayment(ctx, transactionID)
	if err != nil {
		return err
	}
	switch {
	case !found:
		return receiptMismatch("transaction %s does not exist", transactionID)
	case !txn.approved():
		return receiptMismatch("transaction %s is not approved", transactionID)
	case strings.TrimSpace(customerVaultID) == "" || strings.TrimSpace(txn.CustomerVaultID) != strings.TrimSpace(customerVaultID):
		return receiptMismatch("transaction %s is not on the operation's customer vault", transactionID)
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
// approved refund of exactly amount on the original transaction's vault. A
// zero amount means a full refund of the original sale.
func (c *NMIClient) ConfirmRefund(ctx context.Context, originalTransactionID, refundTransactionID string, amount moneyutil.Cents) error {
	original, found, err := c.GetPayment(ctx, originalTransactionID)
	if err != nil {
		return err
	}
	if !found {
		return receiptMismatch("original transaction %s does not exist", originalTransactionID)
	}
	if amount == 0 {
		cents, ok := exactCents(original.Amount)
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
	case !found:
		return receiptMismatch("refund %s does not exist", refundTransactionID)
	case !refund.approved():
		return receiptMismatch("refund %s is not approved", refundTransactionID)
	case strings.TrimSpace(original.CustomerVaultID) == "" || strings.TrimSpace(refund.CustomerVaultID) != strings.TrimSpace(original.CustomerVaultID):
		return receiptMismatch("refund %s is not on the original transaction's customer vault", refundTransactionID)
	case !successfulAction(refund, "refund", amount):
		return receiptMismatch("refund %s has no successful refund of %d cents", refundTransactionID, amount)
	}
	return nil
}

// ConfirmRefundNotExecuted reads the original transaction before accepting an
// operator's non-execution attestation. A successful refund action for the
// requested amount is contradictory evidence and must keep the operation
// unresolved; callers must resolve it from that exact receipt instead.
func (c *NMIClient) ConfirmRefundNotExecuted(ctx context.Context, originalTransactionID string, amount moneyutil.Cents) error {
	txn, found, err := c.GetPayment(ctx, originalTransactionID)
	if err != nil {
		return err
	}
	if !found {
		return receiptMismatch("original transaction %s does not exist", originalTransactionID)
	}
	if amount == 0 {
		cents, ok := exactCents(txn.Amount)
		if !ok || cents <= 0 {
			return receiptMismatch("original transaction %s has no exact amount", originalTransactionID)
		}
		amount = moneyutil.Cents(cents)
	}
	if successfulAction(txn, "refund", amount) {
		return receiptMismatch("original transaction %s contains a successful refund of %d cents", originalTransactionID, amount)
	}
	return nil
}
