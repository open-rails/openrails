package credits

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TierThreshold is one rung of the trust ladder: an account reaches Tier once its
// cumulative PAID spend is at least MinPaidMicros (#298 graduation). Order the
// ladder ascending by MinPaidMicros.
type TierThreshold struct {
	Tier          string
	MinPaidMicros int64
}

// CumulativePaidMicros is the total a (payer, credit_type) has PAID in (sum of
// deposit transactions) — the trust signal that graduates the tier.
func (s *CreditsService) CumulativePaidMicros(ctx context.Context, payer identity.TenantSubjectID, creditType string) (int64, error) {
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return 0, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	var total int64
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		return s.db.Q(ctx).NewSelect().
			Model((*models.CreditTransaction)(nil)).
			ColumnExpr("COALESCE(SUM(amount), 0)").
			Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payer.UUID(), ct.ID).
			Where("transaction_type = 'deposit'").
			Scan(ctx, &total)
	})
	return total, err
}

// GetTier returns the account's graduated tier ("" if none).
func (s *CreditsService) GetTier(ctx context.Context, payer identity.TenantSubjectID, creditType string) (string, error) {
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return "", err
	}
	if settings.Tier == nil {
		return "", nil
	}
	return *settings.Tier, nil
}

// GraduateTier recomputes the account's tier from cumulative paid spend against
// the ladder and persists it (#298). Returns the assigned tier (highest rung
// whose MinPaidMicros the account meets; "" if it meets none).
func (s *CreditsService) GraduateTier(ctx context.Context, payer identity.TenantSubjectID, creditType string, ladder []TierThreshold) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return "", err
	}
	paid, err := s.CumulativePaidMicros(ctx, payer, creditType)
	if err != nil {
		return "", err
	}
	tier := ""
	for _, t := range ladder {
		if paid >= t.MinPaidMicros {
			tier = t.Tier
		}
	}

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	now := s.now()
	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	// Ensure a settings row exists (prepaid mode if creating; no-op when present).
	if err := s.ensureSettingsRowTx(ctx, tx, tenantID, payer.UUID(), ct.ID, BillingModePrepaid, now); err != nil {
		return "", err
	}
	if _, err := tx.NewUpdate().Model((*models.CreditAccountSettings)(nil)).
		Set("tier = ?", tier).
		Set("updated_at = ?", now).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payer.UUID(), ct.ID).
		Exec(ctx); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return tier, nil
}
