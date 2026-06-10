package credits

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
		return fmt.Errorf("credits: decode %s: %w", col, err)
	}
	return nil
}

func toJSONBC[M ~map[string]V, V any](m M) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

func creditTypeFromGen(r gen.BillingCreditType) *models.CreditType {
	return &models.CreditType{
		ID:            r.ID,
		Name:          r.Name,
		DisplayName:   r.DisplayName,
		Unit:          r.Unit,
		DecimalPlaces: int(r.DecimalPlaces),
		IsActive:      r.IsActive,
		CreatedAt:     r.CreatedAt,
	}
}

func creditBalanceFromGen(r gen.BillingCreditBalance) *models.CreditBalance {
	return &models.CreditBalance{
		ID:              r.ID,
		TenantID:        r.TenantID,
		TenantSubjectID: r.TenantSubjectID,
		CreditTypeID:    r.CreditTypeID,
		Balance:         r.Balance,
		HeldBalance:     r.HeldBalance,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

func creditTransactionFromGen(r gen.BillingCreditTransaction) (*models.CreditTransaction, error) {
	m := &models.CreditTransaction{
		ID:              r.ID,
		TenantID:        r.TenantID,
		TenantSubjectID: r.TenantSubjectID,
		Actor:           r.Actor,
		Resource:        r.Resource,
		CreditTypeID:    r.CreditTypeID,
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
	if err := fromJSONBC(r.Metadata, &m.Metadata, "credit_transactions.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func settingsFromGen(r gen.BillingCreditAccountSetting) *models.CreditAccountSettings {
	var expiry *int
	if r.DefaultCreditExpiryDays != nil {
		v := int(*r.DefaultCreditExpiryDays)
		expiry = &v
	}
	return &models.CreditAccountSettings{
		ID:                       r.ID,
		TenantID:                 r.TenantID,
		TenantSubjectID:          r.TenantSubjectID,
		CreditTypeID:             r.CreditTypeID,
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

func spendLimitFromGen(r gen.BillingCreditSpendLimit) *models.CreditSpendLimit {
	return &models.CreditSpendLimit{
		ID:                     r.ID,
		TenantID:               r.TenantID,
		TenantSubjectID:        r.TenantSubjectID,
		CreditTypeID:           r.CreditTypeID,
		Actor:                  r.Actor,
		MaxSpendPerDayMicros:   r.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros: r.MaxSpendPerMonthMicros,
		CreatedAt:              r.CreatedAt,
		UpdatedAt:              r.UpdatedAt,
	}
}

func usageEventFromGen(r gen.BillingUsageEvent) (*models.UsageEvent, error) {
	m := &models.UsageEvent{
		ID:                  r.ID,
		TenantID:            r.TenantID,
		TenantSubjectID:     r.TenantSubjectID,
		Actor:               r.Actor,
		Resource:            r.Resource,
		CreditTypeID:        r.CreditTypeID,
		EventType:           r.EventType,
		Amount:              r.Amount,
		Source:              r.Source,
		SourceID:            r.SourceID,
		CreditTransactionID: r.CreditTransactionID,
		OccurredAt:          r.OccurredAt,
		CreatedAt:           r.CreatedAt,
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
		CreditTypeID:    r.CreditTypeID,
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
			return nil, fmt.Errorf("credits: decode invoices.line_items: %w", err)
		}
	}
	if err := fromJSONBC(r.MoneyMovements, &m.MoneyMovements, "invoices.money_movements"); err != nil {
		return nil, err
	}
	return m, nil
}
