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
	"github.com/open-rails/openrails/pkg/merchant"
)

// TrustLevelThreshold is one rung of the trust ladder: an account reaches
// TrustLevel once its cumulative PAID spend is at least MinPaidAmount (#298
// graduation). Order the ladder ascending by MinPaidAmount. It is also one
// rung of a persisted tier_schedule (#476).
type TrustLevelThreshold struct {
	TrustLevel    string `json:"trust_level"`
	MinPaidAmount int64  `json:"min_cumulative_paid_amount"`
}

// trustLevelForCumulative returns the highest rung whose MinPaidAmount the
// account meets ("" if it meets none). The ladder need not be pre-sorted.
func trustLevelForCumulative(paid int64, ladder []TrustLevelThreshold) string {
	trustLevel := ""
	var best int64 = -1
	for _, t := range ladder {
		if paid >= t.MinPaidAmount && t.MinPaidAmount >= best {
			trustLevel = t.TrustLevel
			best = t.MinPaidAmount
		}
	}
	return trustLevel
}

// CumulativePaidAmount is the total a payer has PAID in one currency (sum of
// credit grants) — the trust signal that graduates that currency's trust level.
func (s *MoneyService) CumulativePaidAmount(ctx context.Context, payer identity.CustomerID, currency string) (int64, error) {
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return 0, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	tenantID := tid.UUID()
	var total int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		total, e = s.db.Gen(ctx).SumCreditGrants(ctx, gen.SumCreditGrantsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
		})
		return e
	})
	return total, err
}

// GetTrustLevel returns the account's graduated trust level for one currency
// ("" if none).
func (s *MoneyService) GetTrustLevel(ctx context.Context, payer identity.CustomerID, currency string) (string, error) {
	settings, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return "", err
	}
	if settings.TrustLevel == nil {
		return "", nil
	}
	return *settings.TrustLevel, nil
}

// SetTrustLevelOverride sets an EXPLICIT admin trust-level override (#476). It
// pins tier_source='admin' so auto-graduation will never overwrite it — the
// admin override always wins over the schedule. An empty trust level clears the
// override back to schedule-driven ('auto') so the next deposit re-derives the
// trust level.
func (s *MoneyService) SetTrustLevelOverride(ctx context.Context, payer identity.CustomerID, currency, trustLevel string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	source := "admin"
	if trustLevel == "" {
		source = "auto"
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), cur, BillingModePrepaid, now); err != nil {
			return err
		}
		return q.SetMoneyAccountTier(ctx, gen.SetMoneyAccountTierParams{
			MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
			Tier: trustLevel, TierSource: source, Now: now,
		})
	})
}

// SetTrustLevelSchedule persists the merchant's trust-level ladder (#476): the
// host declares it ONCE; OpenRails then auto-maintains each payer's trust level
// from cumulative spend. A nil/empty payer writes the merchant-wide default
// schedule; a non-zero payer writes a per-subject override. Schedules are
// platform-owned (the subject cannot see/loosen them — the #473 owner pattern).
func (s *MoneyService) SetTrustLevelSchedule(ctx context.Context, payer identity.CustomerID, currency string, schedule []TrustLevelThreshold) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := s.now()
	rungsJSON, err := json.Marshal(schedule)
	if err != nil {
		return fmt.Errorf("money: encode trust-level schedule: %w", err)
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		if payer.IsZero() {
			return q.UpsertTierScheduleDefault(ctx, gen.UpsertTierScheduleDefaultParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID,
				Currency: cur, Rungs: rungsJSON, CreatedAt: now, UpdatedAt: now,
			})
		}
		// A per-subject override needs the customers FK satisfied.
		if err := ensureCustomer(ctx, q, tenantID, payer.UUID()); err != nil {
			return err
		}
		ts := payer.UUID()
		return q.UpsertTierScheduleSubject(ctx, gen.UpsertTierScheduleSubjectParams{
			ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: &ts,
			Currency: cur, Rungs: rungsJSON, CreatedAt: now, UpdatedAt: now,
		})
	})
}

// GetTrustLevelSchedule returns the EFFECTIVE platform-owned schedule for a
// payer (the subject's override if present, else the merchant-wide default) in
// one currency, or nil when none is set. A zero payer reads the merchant-wide
// default directly.
func (s *MoneyService) GetTrustLevelSchedule(ctx context.Context, payer identity.CustomerID, currency string) ([]TrustLevelThreshold, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	cur := normalizeUnit(currency)
	if err := s.validateUnit(ctx, cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	var out []TrustLevelThreshold
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		// nil subject → only the merchant-wide default row matches (#476).
		row, e := s.db.Gen(ctx).GetEffectiveTierSchedule(ctx, gen.GetEffectiveTierScheduleParams{
			MerchantID: tenantID, Currency: cur, CustomerID: subjectPtr(payer),
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if len(row.Rungs) > 0 {
			if uerr := json.Unmarshal(row.Rungs, &out); uerr != nil {
				return fmt.Errorf("money: decode trust-level schedule: %w", uerr)
			}
		}
		return nil
	})
	return out, err
}

// autoGraduateTrustLevelTx recomputes + persists a payer's trust level from
// cumulative paid spend against the stored schedule, INSIDE the
// deposit/settlement tx (#476). It is EVENTFUL — called where cumulative_paid
// changes — monotonic (only raises, never regresses), and a no-op when no
// schedule exists or the current trust level is an admin override (the DB
// UPDATE itself guards tier_source<>'admin'). `cumPaid` is the post-event
// cumulative; passing it avoids a re-query.
func (s *MoneyService) autoGraduateTrustLevelTx(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, payer identity.CustomerID, currency string, cumPaid int64, now time.Time) error {
	sched, err := s.trustLevelScheduleTx(ctx, q, tenantID, payer, currency)
	if err != nil || len(sched) == 0 {
		return err
	}
	target := trustLevelForCumulative(cumPaid, sched)
	if target == "" {
		return nil
	}
	// Monotonic: never lower the trust level. Compare the target's threshold
	// against the current trust level's threshold in the schedule; only write
	// when target is higher.
	settings, err := s.accountSettingsTx(ctx, q, tenantID, payer, currency)
	if err != nil {
		return err
	}
	if settings != nil && settings.TrustLevelSource == "admin" {
		return nil // admin override wins
	}
	curTrustLevel := ""
	if settings != nil && settings.TrustLevel != nil {
		curTrustLevel = *settings.TrustLevel
	}
	if curTrustLevel != "" && rungThreshold(sched, target) <= rungThreshold(sched, curTrustLevel) {
		return nil // already at or above target — don't regress or churn
	}
	// A deposit may not have created a money_accounts settings row yet (it writes
	// the balance row); ensure one so the AutoGraduate UPDATE has a row to hit.
	if settings == nil {
		if err := s.ensureSettingsRowTx(ctx, q, tenantID, payer.UUID(), currency, BillingModePrepaid, now); err != nil {
			return err
		}
	}
	return q.AutoGraduateMoneyAccountTier(ctx, gen.AutoGraduateMoneyAccountTierParams{
		MerchantID: tenantID, CustomerID: payer.UUID(), Currency: currency,
		Tier: target, Now: now,
	})
}

// subjectPtr maps a payer to the nullable customer_id param: nil for a
// zero payer (matches only the merchant-wide default schedule), else a pointer.
func subjectPtr(payer identity.CustomerID) *uuid.UUID {
	if payer.IsZero() {
		return nil
	}
	u := payer.UUID()
	return &u
}

// rungThreshold returns a trust level's min_cumulative_paid_amount in the
// schedule, or -1 if the trust level is not a rung (an unknown/legacy trust
// level — treated as the lowest so any real rung outranks it).
func rungThreshold(sched []TrustLevelThreshold, trustLevel string) int64 {
	for _, r := range sched {
		if r.TrustLevel == trustLevel {
			return r.MinPaidAmount
		}
	}
	return -1
}

// trustLevelScheduleTx reads the effective platform schedule inside an open tx
// (the connection already carries the merchant GUC, so no RunInMerchantConn wrap).
func (s *MoneyService) trustLevelScheduleTx(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, payer identity.CustomerID, currency string) ([]TrustLevelThreshold, error) {
	row, e := q.GetEffectiveTierSchedule(ctx, gen.GetEffectiveTierScheduleParams{
		MerchantID: tenantID, Currency: currency, CustomerID: subjectPtr(payer),
	})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var out []TrustLevelThreshold
	if len(row.Rungs) > 0 {
		if uerr := json.Unmarshal(row.Rungs, &out); uerr != nil {
			return nil, fmt.Errorf("money: decode trust-level schedule: %w", uerr)
		}
	}
	return out, nil
}

// accountSettingsTx reads the money_accounts row inside an open tx (nil when the
// account has no settings row yet).
func (s *MoneyService) accountSettingsTx(ctx context.Context, q *gen.Queries, tenantID uuid.UUID, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	row, e := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{
		MerchantID: tenantID, CustomerID: payer.UUID(), Currency: currency,
	})
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return settingsFromGen(row), nil
}
