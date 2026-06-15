package money

import (
	"encoding/json"
	"fmt"

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

func moneyTransactionFromGen(r gen.OpenrailsMoneyTransaction) (*models.MoneyTransaction, error) {
	m := &models.MoneyTransaction{
		ID:              r.ID,
		MerchantID:      r.MerchantID,
		CustomerID:      r.CustomerID,
		Currency:        r.Currency,
		Invoker:         r.InvokerID,
		Resource:        r.Resource,
		Amount:          r.Amount,
		BalanceAfter:    r.BalanceAfter,
		TransactionType: r.TransactionType,
		Status:          r.Status,
		Authorized:      r.AuthorizedAmount,
		Captured:        r.CapturedAmount,
		Source:          r.Source,
		SourceID:        r.SourceID,
		ExpiresAt:       r.ExpiresAt,
		Description:     r.Description,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
	if err := fromJSONBC(r.Metadata, &m.Metadata, "money_transactions.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func settingsFromGen(r gen.OpenrailsMoneySetting) *models.MoneyAccount {
	var expiry *int
	if r.DefaultCreditExpiryDays != nil {
		v := int(*r.DefaultCreditExpiryDays)
		expiry = &v
	}
	return &models.MoneyAccount{
		ID:                       r.ID,
		MerchantID:               r.MerchantID,
		CustomerID:               r.CustomerID,
		Currency:                 r.Currency,
		BillingMode:              r.BillingMode,
		MaxSpendPerDay:           r.MaxSpendPerDay,
		MaxSpendPerMonth:         r.MaxSpendPerMonth,
		MaxOutstandingOwedAmount: r.MaxOutstandingOwedAmount,
		LowBalanceThreshold:      r.LowBalanceThreshold,
		AutoTopupEnabled:         r.AutoTopupEnabled,
		AutoTopupAmountCents:     r.AutoTopupAmountCents,
		AutoTopupPaymentMethod:   r.AutoTopupPaymentMethodID,
		DefaultCreditExpiryDays:  expiry,
		HardStopOnBreach:         r.HardStopOnBreach,
		AlertThresholdPct:        int(r.AlertThresholdPct),
		OutstandingOwedAmount:    r.OutstandingOwedAmount,
		CreditLimitAmount:        r.CreditLimitAmount,
		LastAlertAt:              r.LastAlertAt,
		LastTopupAt:              r.LastTopupAt,
		VerifiedPaymentMethod:    r.VerifiedPaymentMethod,
		VerifiedAt:               r.VerifiedAt,
		SuspendedAt:              r.SuspendedAt,
		SuspendReason:            r.SuspendReason,
		Tier:                     r.Tier,
		TierSource:               r.TierSource,
		CreatedAt:                r.CreatedAt,
		UpdatedAt:                r.UpdatedAt,
	}
}

func spendLimitFromGen(r gen.OpenrailsMoneySpendLimit) *models.MoneySpendLimit {
	return &models.MoneySpendLimit{
		ID:               r.ID,
		MerchantID:       r.MerchantID,
		CustomerID:       r.CustomerID,
		Currency:         r.Currency,
		Invoker:          r.InvokerID,
		MaxSpendPerDay:   r.MaxSpendPerDay,
		MaxSpendPerMonth: r.MaxSpendPerMonth,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

func usageEventFromGen(r gen.OpenrailsUsageEvent) (*models.UsageEvent, error) {
	m := &models.UsageEvent{
		ID:                 r.ID,
		MerchantID:         r.MerchantID,
		CustomerID:         r.CustomerID,
		Invoker:            r.InvokerID,
		Currency:           r.Currency,
		Resource:           r.Resource,
		EventType:          r.EventType,
		Amount:             r.Amount,
		Source:             r.Source,
		SourceID:           r.SourceID,
		MoneyTransactionID: r.MoneyTransactionID,
		OccurredAt:         r.OccurredAt,
		CreatedAt:          r.CreatedAt,
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
		ID:             r.ID,
		MerchantID:     r.MerchantID,
		CustomerID:     r.CustomerID,
		Currency:       r.Currency,
		PeriodFrom:     r.PeriodFrom,
		PeriodTo:       r.PeriodTo,
		UsageTotal:     r.UsageTotal,
		DepositsTotal:  r.DepositsTotal,
		OwedAccrued:    r.OwedAccrued,
		OwedPaid:       r.OwedPaid,
		ClosingBalance: r.ClosingBalance,
		Status:         r.Status,
		FinalizedAt:    r.FinalizedAt,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if len(r.LineItems) > 0 {
		if err := json.Unmarshal(r.LineItems, &m.LineItems); err != nil {
			return nil, fmt.Errorf("money: decode invoices.line_items: %w", err)
		}
	}
	if err := fromJSONBC(r.MoneyMovements, &m.MoneyMovements, "invoices.money_movements"); err != nil {
		return nil, err
	}
	return m, nil
}
