package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// TierThreshold is one rung of the trust ladder: an account reaches Tier once its
// cumulative PAID spend is at least MinPaidMicros (#298 graduation). Order the
// ladder ascending by MinPaidMicros. It is also one rung of a persisted
// tier_schedule (#476).
type TierThreshold struct {
	Tier          string `json:"tier"`
	MinPaidMicros int64  `json:"min_cumulative_paid_micros"`
}

// tierForCumulative returns the highest rung whose MinPaidMicros the account
// meets ("" if it meets none). The ladder need not be pre-sorted.
func tierForCumulative(paid int64, ladder []TierThreshold) string {
	tier := ""
	var best int64 = -1
	for _, t := range ladder {
		if paid >= t.MinPaidMicros && t.MinPaidMicros >= best {
			tier = t.Tier
			best = t.MinPaidMicros
		}
	}
	return tier
}

// CumulativePaidMicros is the total a payer has PAID in (sum of deposit
// transactions) — the trust signal that graduates the tier.
func (s *MoneyService) CumulativePaidMicros(ctx context.Context, payer identity.TenantSubjectID) (int64, error) {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	var total int64
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		var e error
		total, e = s.db.Gen(ctx).SumMoneyDeposits(ctx, gen.SumMoneyDepositsParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), Currency: DefaultCurrency,
		})
		return e
	})
	return total, err
}

// GetTier returns the account's graduated tier ("" if none).
func (s *MoneyService) GetTier(ctx context.Context, payer identity.TenantSubjectID) (string, error) {
	settings, err := s.GetAccountSettings(ctx, payer)
	if err != nil {
		return "", err
	}
	if settings.Tier == nil {
		return "", nil
	}
	return *settings.Tier, nil
}

// GraduateTier recomputes the account's tier from cumulative paid spend against
// the HOST-SUPPLIED ladder and persists it (#298 legacy host-cranked path).
//
// DEPRECATED (#476): when a persisted tier_schedule exists for the tenant,
// OpenRails OWNS graduation (auto-maintained on the deposit/settlement path), so
// this becomes a NO-OP that returns the current auto-maintained tier — the host
// no longer needs to crank it. With no schedule, it keeps the legacy behavior so
// existing hosts keep working.
func (s *MoneyService) GraduateTier(ctx context.Context, payer identity.TenantSubjectID, ladder []TierThreshold) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("money service not initialized")
	}
	// Schedule present → OpenRails owns it; host crank is a no-op.
	if sched, err := s.GetTierSchedule(ctx, payer); err != nil {
		return "", err
	} else if len(sched) > 0 {
		return s.GetTier(ctx, payer)
	}

	paid, err := s.CumulativePaidMicros(ctx, payer)
	if err != nil {
		return "", err
	}
	tier := tierForCumulative(paid, ladder)

	tid, err := tenant.Require(ctx)
	if err != nil {
		return "", err
	}
	tenantID := tid.UUID()
	now := s.now()
	err = s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// Ensure a settings row exists (prepaid mode if creating; no-op when present).
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), DefaultCurrency, BillingModePrepaid, now); err != nil {
			return err
		}
		// Legacy host-cranked path writes tier_source='auto' (it is not an admin
		// override; a later schedule + auto-graduation may revise it).
		return q.SetMoneyAccountTier(ctx, gen.SetMoneyAccountTierParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), Currency: DefaultCurrency,
			Tier: tier, TierSource: "auto", Now: now,
		})
	})
	if err != nil {
		return "", err
	}
	return tier, nil
}

// SetTierOverride sets an EXPLICIT admin tier override (#476). It pins
// tier_source='admin' so auto-graduation will never overwrite it — the admin
// override always wins over the schedule. An empty tier clears the override back
// to schedule-driven ('auto') so the next deposit re-derives the tier.
func (s *MoneyService) SetTierOverride(ctx context.Context, payer identity.TenantSubjectID, tier string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	source := "admin"
	if tier == "" {
		source = "auto"
	}
	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), DefaultCurrency, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SetMoneyAccountTier(ctx, gen.SetMoneyAccountTierParams{
			TenantID: tenantID, TenantSubjectID: payer.UUID(), Currency: DefaultCurrency,
			Tier: tier, TierSource: source, Now: now,
		})
	})
}

// SetTierSchedule persists the tenant's tier ladder (#476): the host declares it
// ONCE; OpenRails then auto-maintains each payer's tier from cumulative spend.
// A nil/empty payer writes the tenant-wide default schedule; a non-zero payer
// writes a per-subject override. owner is forced to 'platform' (set by us; the
// subject cannot see/loosen it — the #473 owner pattern).
func (s *MoneyService) SetTierSchedule(ctx context.Context, payer identity.TenantSubjectID, schedule []TierThreshold) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	rungsJSON, err := json.Marshal(schedule)
	if err != nil {
		return fmt.Errorf("money: encode tier schedule: %w", err)
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		if payer.IsZero() {
			return q.UpsertTierScheduleDefault(ctx, gen.UpsertTierScheduleDefaultParams{
				ID: uuidutil.NewV7(), TenantID: tenantID, Owner: "platform",
				Rungs: rungsJSON, CreatedAt: now, UpdatedAt: now,
			})
		}
		// A per-subject override needs the tenant_subjects FK satisfied.
		if err := ensureTenantSubject(ctx, q, tenantID, payer.UUID()); err != nil {
			return err
		}
		ts := payer.UUID()
		return q.UpsertTierScheduleSubject(ctx, gen.UpsertTierScheduleSubjectParams{
			ID: uuidutil.NewV7(), TenantID: tenantID, TenantSubjectID: &ts, Owner: "platform",
			Rungs: rungsJSON, CreatedAt: now, UpdatedAt: now,
		})
	})
}

// GetTierSchedule returns the EFFECTIVE platform-owned schedule for a payer (the
// subject's override if present, else the tenant-wide default), or nil when none
// is set. A zero payer reads the tenant-wide default directly.
func (s *MoneyService) GetTierSchedule(ctx context.Context, payer identity.TenantSubjectID) ([]TierThreshold, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	var out []TierThreshold
	err = s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		// nil subject → only the tenant-wide default row matches (#476).
		row, e := s.db.Gen(ctx).GetEffectiveTierSchedule(ctx, gen.GetEffectiveTierScheduleParams{
			TenantID: tenantID, Owner: "platform", TenantSubjectID: subjectPtr(payer),
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if len(row.Rungs) > 0 {
			if uerr := json.Unmarshal(row.Rungs, &out); uerr != nil {
				return fmt.Errorf("money: decode tier schedule: %w", uerr)
			}
		}
		return nil
	})
	return out, err
}

// autoGraduateTierTx recomputes + persists a payer's tier from cumulative paid
// spend against the stored schedule, INSIDE the deposit/settlement tx (#476). It
// is EVENTFUL — called where cumulative_paid changes — monotonic (only raises,
// never regresses), and a no-op when no schedule exists or the current tier is an
// admin override (the DB UPDATE itself guards tier_source<>'admin'). `cumPaid` is
// the post-event cumulative; passing it avoids a re-query.
func (s *MoneyService) autoGraduateTierTx(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, payer identity.TenantSubjectID, cumPaid int64, now time.Time) error {
	sched, err := s.tierScheduleTx(ctx, q, tenantID, payer)
	if err != nil || len(sched) == 0 {
		return err
	}
	target := tierForCumulative(cumPaid, sched)
	if target == "" {
		return nil
	}
	// Monotonic: never lower the tier. Compare the target's threshold against the
	// current tier's threshold in the schedule; only write when target is higher.
	settings, err := s.accountSettingsTx(ctx, q, tenantID, payer)
	if err != nil {
		return err
	}
	if settings != nil && settings.TierSource == "admin" {
		return nil // admin override wins
	}
	curTier := ""
	if settings != nil && settings.Tier != nil {
		curTier = *settings.Tier
	}
	if curTier != "" && rungThreshold(sched, target) <= rungThreshold(sched, curTier) {
		return nil // already at or above target — don't regress or churn
	}
	// A deposit may not have created a money_accounts settings row yet (it writes
	// the balance row); ensure one so the AutoGraduate UPDATE has a row to hit.
	if settings == nil {
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), DefaultCurrency, BillingModePrepaid, now); err != nil {
			return err
		}
	}
	return q.AutoGraduateMoneyAccountTier(ctx, gen.AutoGraduateMoneyAccountTierParams{
		TenantID: tenantID, TenantSubjectID: payer.UUID(), Currency: DefaultCurrency,
		Tier: target, Now: now,
	})
}

// subjectPtr maps a payer to the nullable tenant_subject_id param: nil for a
// zero payer (matches only the tenant-wide default schedule), else a pointer.
func subjectPtr(payer identity.TenantSubjectID) *uuid.UUID {
	if payer.IsZero() {
		return nil
	}
	u := payer.UUID()
	return &u
}

// rungThreshold returns a tier's min_cumulative_paid_micros in the schedule, or
// -1 if the tier is not a rung (an unknown/legacy tier — treated as the lowest so
// any real rung outranks it).
func rungThreshold(sched []TierThreshold, tier string) int64 {
	for _, r := range sched {
		if r.Tier == tier {
			return r.MinPaidMicros
		}
	}
	return -1
}

// tierScheduleTx reads the effective platform schedule inside an open tx (the
// connection already carries the tenant GUC, so no RunInTenantConn wrap).
func (s *MoneyService) tierScheduleTx(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, payer identity.TenantSubjectID) ([]TierThreshold, error) {
	row, e := q.GetEffectiveTierSchedule(ctx, gen.GetEffectiveTierScheduleParams{
		TenantID: tenantID, Owner: "platform", TenantSubjectID: subjectPtr(payer),
	})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var out []TierThreshold
	if len(row.Rungs) > 0 {
		if uerr := json.Unmarshal(row.Rungs, &out); uerr != nil {
			return nil, fmt.Errorf("money: decode tier schedule: %w", uerr)
		}
	}
	return out, nil
}

// accountSettingsTx reads the money_accounts row inside an open tx (nil when the
// account has no settings row yet).
func (s *MoneyService) accountSettingsTx(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, payer identity.TenantSubjectID) (*models.MoneyAccount, error) {
	row, e := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{
		TenantID: tenantID, TenantSubjectID: payer.UUID(), Currency: DefaultCurrency,
	})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return settingsFromGen(row), nil
}
