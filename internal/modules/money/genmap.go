package money

import (
	"encoding/json"
	"fmt"

	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Mapping helpers between sqlc-generated row types (internal/db/gen) and the
// domain models this service returns: gen types never leak out of the package.

func fromJSONBC[T any](b []byte, dst *T, col string) error {
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("money: decode %s: %w", col, err)
	}
	return nil
}

func toJSONBC[M ~map[string]V, V any](m M) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// moneyTransactionFromTransfer derives the public MoneyTransaction DTO from an
// immutable ledger transfer. Money out of the customer is negative; UpdatedAt
// == CreatedAt.
func moneyTransactionFromTransfer(r gen.BillingLedgerTransfer) *models.MoneyTransaction {
	amount := r.Amount
	txType := r.TransferType
	switch r.TransferType {
	case "deposit", "deposit_bonus":
		txType = "deposit"
	case "credit_spend", "spend", "capture":
		amount = -amount
		txType = "withdrawal"
	case "owed_accrual":
		txType = "owed_accrual"
	case "owed_payment", "owed_repayment":
		amount = -amount
	case "credit_expire", "expire":
		amount = -amount
		txType = "expiry"
	case "credit_revoke", "credit_refund":
		amount = -amount
	}
	// Every ledger transfer is posted.
	status := "posted"
	var customerID uuid.UUID
	if r.CustomerID != nil {
		customerID = *r.CustomerID
	}
	return &models.MoneyTransaction{
		ID:              r.ID,
		MerchantID:      r.MerchantID,
		CustomerID:      customerID,
		Currency:        r.Currency,
		Invoker:         derefStr(r.InvokerID),
		Resource:        r.Resource,
		Amount:          amount,
		TransactionType: txType,
		Status:          status,
		Source:          r.Source,
		SourceID:        &r.SourceID,
		InvoiceID:       r.InvoiceID,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.CreatedAt,
	}
}

// balanceTransactionFromTransfer is a customer's view of one ledger transfer:
// its type, and its amount signed as the change to the balance (to what is
// owed, for owed_ types).
func balanceTransactionFromTransfer(r gen.BillingLedgerTransfer) billing.BalanceTransaction {
	txType, amount := billing.BalanceTransactionType(r.TransferType), r.Amount
	switch r.TransferType {
	case "deposit_bonus":
		txType = billing.BalanceTransactionDeposit
	case "credit_refund":
		txType, amount = billing.BalanceTransactionRevoke, -amount
	case "credit_refund_restore":
		txType = billing.BalanceTransactionReinstate
	case "credit_spend":
		txType, amount = billing.BalanceTransactionSpend, -amount
	case "credit_expire":
		txType, amount = billing.BalanceTransactionExpire, -amount
	case "credit_revoke":
		txType, amount = billing.BalanceTransactionRevoke, -amount
	case "credit_reinstate":
		txType = billing.BalanceTransactionReinstate
	case "owed_payment", "owed_writeoff", "owed_repayment":
		amount = -amount
	}
	out := billing.BalanceTransaction{
		ID: billing.BalanceTransactionID(r.ID), Currency: r.Currency, Type: txType, Amount: amount,
		Invoker: r.InvokerID, Resource: r.Resource, Source: r.Source, SourceID: r.SourceID, CreatedAt: r.CreatedAt,
	}
	if r.CustomerID != nil {
		out.CustomerID = billing.CustomerID(*r.CustomerID)
	}
	if r.GrantID != nil {
		grant := billing.CreditGrantID(*r.GrantID)
		out.CreditGrantID = &grant
	}
	return out
}

func settingsFromGen(r gen.BillingMoneySetting) *models.MoneyAccount {
	return &models.MoneyAccount{
		MerchantID:           r.MerchantID,
		CustomerID:           r.CustomerID,
		Currency:             r.Currency,
		BillingMode:          r.BillingMode,
		DefaultPaymentMethod: r.DefaultPaymentMethodID,
		CreditLimitAmount:    r.CreditLimitAmount,
		TrustLevel:           r.Tier,
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
	}
}

func usageEventFromGen(r gen.BillingUsageEvent) (*models.UsageEvent, error) {
	m := &models.UsageEvent{
		ID:               r.ID,
		MerchantID:       r.MerchantID,
		CustomerID:       r.CustomerID,
		Invoker:          r.InvokerID,
		Currency:         r.Currency,
		Resource:         r.Resource,
		EventType:        r.EventType,
		Amount:           r.Amount,
		Outcome:          r.Outcome,
		ForgivenAmount:   r.ForgivenAmount,
		Source:           r.Source,
		SourceID:         r.SourceID,
		LedgerTransferID: r.LedgerTransferID,
		PricingAuthority: r.PricingAuthority,
		OccurredAt:       r.OccurredAt,
		CreatedAt:        r.CreatedAt,
	}
	if err := fromJSONBC(r.Dimensions, &m.Dimensions, "usage_events.dimensions"); err != nil {
		return nil, err
	}
	if err := fromJSONBC(r.Metadata, &m.Metadata, "usage_events.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func invoiceFromGen(r gen.BillingInvoice) (*models.Invoice, error) {
	m := &models.Invoice{
		ID:                           r.ID,
		MerchantID:                   r.MerchantID,
		CustomerID:                   r.CustomerID,
		Currency:                     r.Currency,
		InvoiceNumber:                r.InvoiceNumber,
		PeriodStartsAt:               r.PeriodStartsAt,
		PeriodEndsAt:                 r.PeriodEndsAt,
		UsageTotal:                   r.UsageTotal,
		DepositsTotal:                r.DepositsTotal,
		OwedAccrued:                  r.OwedAccrued,
		OwedPaid:                     r.OwedPaid,
		ClosingBalance:               r.ClosingBalance,
		SubtotalAmount:               r.SubtotalAmount,
		TotalAmount:                  r.TotalAmount,
		AmountPaid:                   r.AmountPaid,
		AmountDue:                    r.AmountDue,
		PONumber:                     r.PoNumber,
		Memo:                         r.Memo,
		Status:                       r.Status,
		CollectionMethod:             r.CollectionMethod,
		IssuedAt:                     r.IssuedAt,
		DueAt:                        r.DueAt,
		PaidAt:                       r.PaidAt,
		VoidedAt:                     r.VoidedAt,
		UncollectibleAt:              r.UncollectibleAt,
		FinalizedAt:                  r.FinalizedAt,
		ExternalInvoiceID:            r.ExternalInvoiceID,
		CollectionFailureCount:       r.CollectionFailureCount,
		CollectionAttemptCount:       r.CollectionAttemptCount,
		CollectionFailedAt:           r.CollectionFailedAt,
		NextCollectionAttemptAt:      r.NextCollectionAttemptAt,
		LastCollectionFailureCode:    r.LastCollectionFailureCode,
		LastCollectionFailureMessage: r.LastCollectionFailureMessage,
		CollectionIntentID:           r.CollectionIntentID,
		CreatedAt:                    r.CreatedAt,
		UpdatedAt:                    r.UpdatedAt,
	}
	if len(r.LineItems) > 0 {
		if err := json.Unmarshal(r.LineItems, &m.LineItems); err != nil {
			return nil, fmt.Errorf("money: decode invoices.line_items: %w", err)
		}
	}
	if err := fromJSONBC(r.MoneyMovements, &m.MoneyMovements, "invoices.money_movements"); err != nil {
		return nil, err
	}
	if err := fromJSONBC(r.Tax, &m.Tax, "invoices.tax"); err != nil {
		return nil, err
	}
	if len(r.BillingContacts) > 0 {
		if err := json.Unmarshal(r.BillingContacts, &m.BillingContacts); err != nil {
			return nil, fmt.Errorf("money: decode invoices.billing_contacts: %w", err)
		}
	}
	return m, nil
}
