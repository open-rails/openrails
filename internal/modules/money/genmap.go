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

func moneyBalanceFromGen(r gen.BillingMoneyBalance) *models.MoneyBalance {
	return &models.MoneyBalance{
		ID:              r.ID,
		TenantID:        r.TenantID,
		TenantSubjectID: r.TenantSubjectID,
		Balance:         r.Balance,
		HeldBalance:     r.HeldBalance,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

func moneyTransactionFromGen(r gen.BillingMoneyTransaction) (*models.MoneyTransaction, error) {
	m := &models.MoneyTransaction{
		ID:              r.ID,
		TenantID:        r.TenantID,
		TenantSubjectID: r.TenantSubjectID,
		Actor:           r.Actor,
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

func settingsFromGen(r gen.BillingMoneyAccount) *models.MoneyAccount {
	var expiry *int
	if r.DefaultCreditExpiryDays != nil {
		v := int(*r.DefaultCreditExpiryDays)
		expiry = &v
	}
	return &models.MoneyAccount{
		ID:                       r.ID,
		TenantID:                 r.TenantID,
		TenantSubjectID:          r.TenantSubjectID,
		BillingMode:              r.BillingMode,
		MaxSpendPerDayMicros:     r.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros:   r.MaxSpendPerMonthMicros,
		MaxOutstandingOwedMicros: r.MaxOutstandingOwedMicros,
		LowBalanceThreshold:      r.LowBalanceThresholdMicros,
		AutoTopupEnabled:         r.AutoTopupEnabled,
		AutoTopupAmountCents:     r.AutoTopupAmountCents,
		AutoTopupPaymentMethod:   r.AutoTopupPaymentMethodID,
		DefaultCreditExpiryDays:  expiry,
		HardStopOnBreach:         r.HardStopOnBreach,
		AlertThresholdPct:        int(r.AlertThresholdPct),
		OutstandingOwedMicros:    r.OutstandingOwedMicros,
		LastAlertAt:              r.LastAlertAt,
		LastTopupAt:              r.LastTopupAt,
		VerifiedPaymentMethod:    r.VerifiedPaymentMethod,
		VerifiedAt:               r.VerifiedAt,
		SuspendedAt:              r.SuspendedAt,
		SuspendReason:            r.SuspendReason,
		Tier:                     r.Tier,
		CreatedAt:                r.CreatedAt,
		UpdatedAt:                r.UpdatedAt,
	}
}

func spendLimitFromGen(r gen.BillingMoneySpendLimit) *models.MoneySpendLimit {
	return &models.MoneySpendLimit{
		ID:                     r.ID,
		TenantID:               r.TenantID,
		TenantSubjectID:        r.TenantSubjectID,
		Actor:                  r.Actor,
		MaxSpendPerDayMicros:   r.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros: r.MaxSpendPerMonthMicros,
		CreatedAt:              r.CreatedAt,
		UpdatedAt:              r.UpdatedAt,
	}
}

func usageEventFromGen(r gen.BillingUsageEvent) (*models.UsageEvent, error) {
	m := &models.UsageEvent{
		ID:                 r.ID,
		TenantID:           r.TenantID,
		TenantSubjectID:    r.TenantSubjectID,
		Actor:              r.Actor,
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

func invoiceFromGen(r gen.BillingInvoice) (*models.Invoice, error) {
	m := &models.Invoice{
		ID:              r.ID,
		TenantID:        r.TenantID,
		TenantSubjectID: r.TenantSubjectID,
		Currency:        r.Currency,
		PeriodFrom:      r.PeriodFrom,
		PeriodTo:        r.PeriodTo,
		UsageTotal:      r.UsageTotal,
		DepositsTotal:   r.DepositsTotal,
		OwedAccrued:     r.OwedAccrued,
		OwedPaid:        r.OwedPaid,
		ClosingBalance:  r.ClosingBalance,
		Status:          r.Status,
		FinalizedAt:     r.FinalizedAt,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
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
