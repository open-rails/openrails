package money

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Mapping helpers between sqlc-generated row types (internal/db/gen) and the
// domain models this service returns (#334 boundary rule: gen types never
// leak out of the package).

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

// moneyTransactionFromTransfer derives the public MoneyTransaction DTO from a
// #512 immutable ledger transfer (the single-entry money_transactions table was
// retired in the hard cut). The DTO's sign convention is preserved: money OUT of
// the customer is negative. Transfers are immutable, so UpdatedAt == CreatedAt.
func moneyTransactionFromTransfer(r gen.OpenrailsLedgerTransfer) *models.MoneyTransaction {
	amount := r.Amount
	txType := r.TransferType
	switch r.TransferType {
	case "deposit":
		txType = "deposit"
	case "credit_spend", "spend", "capture":
		amount = -amount
		txType = "withdrawal"
	case "owed_accrual":
		txType = "owed_accrual"
	case "owed_payment":
		amount = -amount
		txType = "owed_payment"
	case "credit_expire", "expire":
		amount = -amount
		txType = "expiry"
	case "credit_revoke":
		amount = -amount
	}
	// Every ledger transfer is posted (single-phase) since migration 014 retired
	// the in-ledger two-phase pending apparatus (holds are Redis-only, #513).
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

func settingsFromGen(r gen.OpenrailsMoneySetting) *models.MoneyAccount {
	var expiry *int
	if r.DefaultCreditExpiryHours != nil {
		v := int(*r.DefaultCreditExpiryHours)
		expiry = &v
	}
	return &models.MoneyAccount{
		MerchantID:               r.MerchantID,
		CustomerID:               r.CustomerID,
		Currency:                 r.Currency,
		BillingMode:              r.BillingMode,
		LowBalanceThreshold:      r.LowBalanceThreshold,
		AutoTopupEnabled:         r.AutoTopupEnabled,
		AutoTopupAmount:          r.AutoTopupAmount,
		AutoTopupPaymentMethod:   r.AutoTopupPaymentMethodID,
		CollectionPaymentMethod:  r.CollectionPaymentMethodID,
		DefaultCreditExpiryHours: expiry,
		CreditLimitAmount:        r.CreditLimitAmount,
		LastTopupAt:              r.LastTopupAt,
		TrustLevel:               r.Tier,
		TrustLevelSource:         r.TierSource,
		CreatedAt:                r.CreatedAt,
		UpdatedAt:                r.UpdatedAt,
	}
}

func usageEventFromGen(r gen.OpenrailsUsageEvent) (*models.UsageEvent, error) {
	m := &models.UsageEvent{
		ID:               r.ID,
		MerchantID:       r.MerchantID,
		CustomerID:       r.CustomerID,
		Invoker:          r.InvokerID,
		Currency:         r.Currency,
		Resource:         r.Resource,
		EventType:        r.EventType,
		Amount:           r.Amount,
		Source:           r.Source,
		SourceID:         r.SourceID,
		LedgerTransferID: r.LedgerTransferID,
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

func invoiceFromGen(r gen.OpenrailsInvoice) (*models.Invoice, error) {
	m := &models.Invoice{
		ID:                           r.ID,
		MerchantID:                   r.MerchantID,
		CustomerID:                   r.CustomerID,
		Currency:                     r.Currency,
		InvoiceNumber:                r.InvoiceNumber,
		PeriodFrom:                   r.PeriodFrom,
		PeriodTo:                     r.PeriodTo,
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
		CollectionFailedAt:           r.CollectionFailedAt,
		NextCollectionAttemptAt:      r.NextCollectionAttemptAt,
		LastCollectionFailureCode:    r.LastCollectionFailureCode,
		LastCollectionFailureMessage: r.LastCollectionFailureMessage,
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

func invoicePaymentAttemptFromGen(r gen.OpenrailsInvoicePayment) models.InvoicePaymentAttempt {
	return models.InvoicePaymentAttempt{
		ID:              r.ID,
		InvoiceID:       r.InvoiceID,
		Currency:        r.Currency,
		Amount:          r.Amount,
		Status:          r.Status,
		PaymentMethodID: r.PaymentMethodID,
		Rail:            r.Rail,
		RailPaymentID:   r.RailPaymentID,
		FailureCode:     r.FailureCode,
		FailureReason:   r.FailureReason,
		FailureMessage:  r.FailureMessage,
		AttemptedAt:     r.AttemptedAt,
		SettledAt:       r.SettledAt,
	}
}
