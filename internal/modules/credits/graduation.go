package credits

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
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
	tid, err := tenant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	var total int64
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		var e error
		total, e = s.db.Gen(ctx).SumCreditDeposits(ctx, gen.SumCreditDepositsParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), CreditTypeID: ct.ID,
		})
		return e
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

	tid, err := tenant.Require(ctx)
	if err != nil {
		return "", err
	}
	tenantID := tid.UUID()
	now := s.now()
	err = s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// Ensure a settings row exists (prepaid mode if creating; no-op when present).
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), ct.ID, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SetCreditAccountTier(ctx, gen.SetCreditAccountTierParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), CreditTypeID: ct.ID,
			Tier: tier, Now: now,
		})
	})
	if err != nil {
		return "", err
	}
	return tier, nil
}
