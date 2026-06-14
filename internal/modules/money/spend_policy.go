package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	safecast "github.com/ccoveille/go-safecast/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Billing modes.
const (
	BillingModePrepaid = "prepaid"
	BillingModeArrears = "arrears"
)

// Spend deny codes (the shared contract surfaced as the deny_code on
// authorize/hold and mapped to a 402 by callers — issue #237/#238).
const (
	DenyActorDailyCap   = "actor_daily_cap_exceeded"
	DenyActorMonthlyCap = "actor_monthly_cap_exceeded"
	DenyDailyCap        = "daily_cap_exceeded"
	DenyMonthlyCap      = "monthly_cap_exceeded"
	DenyOutstandingCap  = "outstanding_cap_exceeded"
)

// CapResult is the evaluation of a single cap for a spend request.
type CapResult struct {
	Code           string     `json:"code"`
	Limit          int64      `json:"limit_micros"`
	Spent          int64      `json:"spent_micros"`     // spend already counted in the window/exposure
	Projected      int64      `json:"projected_micros"` // spent + estimate
	Remaining      int64      `json:"remaining_micros"` // limit - spent (may be negative)
	Allowed        bool       `json:"allowed"`
	UtilizationPct int        `json:"utilization_pct"` // projected*100/limit, capped at 1000
	ResetAt        *time.Time `json:"reset_at,omitempty"`
}

// SpendDecision is the outcome of a CheckSpendAllowed evaluation.
type SpendDecision struct {
	Allowed           bool        `json:"allowed"`
	HardStop          bool        `json:"hard_stop"` // whether a breach hard-blocks (vs warn-only)
	DenyCode          string      `json:"deny_code,omitempty"`
	RetryAfterSeconds int64       `json:"retry_after_seconds,omitempty"`
	NextAllowedAt     *time.Time  `json:"next_allowed_at,omitempty"`
	Caps              []CapResult `json:"caps,omitempty"`
	// AlertCode is the first cap at/over the alert threshold (e.g. 80%), for the
	// approaching-limit signal (#238). Empty when nothing is near a cap.
	AlertCode string `json:"alert_code,omitempty"`
}

// capInput is the DB-free input for one cap.
type capInput struct {
	code    string
	limit   *int64 // nil = no cap
	spent   int64
	resetAt *time.Time
}

// evaluateSpend is the PURE spend-policy decision (no DB, no clock side effects):
// it projects each cap's usage as spent+estimate and returns the binding deny (if
// any). Caps are evaluated in the order given; the first violated cap is the
// binding one. hardStop=false makes a breach warn-only (Allowed stays true).
// alertPct (0..100) marks the first cap at/over that utilization as AlertCode.
func evaluateSpend(caps []capInput, estimate int64, hardStop bool, alertPct int, now time.Time) SpendDecision {
	dec := SpendDecision{Allowed: true, HardStop: hardStop}
	if estimate < 0 {
		estimate = 0
	}
	for _, c := range caps {
		if c.limit == nil {
			continue
		}
		limit := *c.limit
		projected := c.spent + estimate
		res := CapResult{
			Code:      c.code,
			Limit:     limit,
			Spent:     c.spent,
			Projected: projected,
			Remaining: limit - c.spent,
			Allowed:   projected <= limit,
			ResetAt:   c.resetAt,
		}
		if limit > 0 {
			u := max(int64(0), min(int64(1000), projected*100/limit))
			res.UtilizationPct = int(u)
		} else {
			// A zero cap means "no spend permitted"; any estimate breaches it.
			if projected > 0 {
				res.UtilizationPct = 1000
			}
		}
		dec.Caps = append(dec.Caps, res)

		if alertPct > 0 && dec.AlertCode == "" && res.UtilizationPct >= alertPct {
			dec.AlertCode = c.code
		}

		if !res.Allowed && dec.DenyCode == "" {
			// Bind to the first violated cap.
			dec.DenyCode = c.code
			if hardStop {
				dec.Allowed = false
			}
			if c.resetAt != nil {
				secs := max(int64(0), int64(c.resetAt.Sub(now).Seconds()))
				dec.RetryAfterSeconds = secs
				dec.NextAllowedAt = c.resetAt
			}
		}
	}
	return dec
}

// dayWindow returns the UTC calendar-day [start, reset) that now falls in.
func dayWindow(now time.Time) (start, reset time.Time) {
	u := now.UTC()
	start = time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	reset = start.AddDate(0, 0, 1)
	return
}

// monthWindow returns the UTC calendar-month [start, reset) that now falls in.
func monthWindow(now time.Time) (start, reset time.Time) {
	u := now.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	reset = start.AddDate(0, 1, 0)
	return
}

// DefaultAccountSettings returns the implicit policy for an payer that has no
// explicit settings row: prepaid, no caps, hard-stop on, 80% alerts, 365-day
// default expiry. Used by GetAccountSettings and the enforcement path so an
// unconfigured account "just works" (prepaid, balance-gated only).
func DefaultAccountSettings(payer identity.TenantSubjectID) *models.MoneyAccount {
	days := 365
	return &models.MoneyAccount{
		TenantSubjectID:         payer.UUID(),
		BillingMode:             BillingModePrepaid,
		HardStopOnBreach:        true,
		AlertThresholdPct:       80,
		DefaultCreditExpiryDays: &days,
	}
}

// GetAccountSettings returns the stored settings for (payer, credit_type), or a
// DefaultAccountSettings value when none exists (never nil, never an error for a
// missing row).
func (s *MoneyService) GetAccountSettings(ctx context.Context, payer identity.TenantSubjectID) (*models.MoneyAccount, error) {
	return s.getAccountSettings(ctx, payer, DefaultCurrency)
}

// getAccountSettings is the currency-aware form (#472).
func (s *MoneyService) getAccountSettings(ctx context.Context, payer identity.TenantSubjectID, currency string) (*models.MoneyAccount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	// Account settings / owed / auto-topup are billing-layer (#475 invariant):
	// custom credit units are never billed in.
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	row, err := s.db.Gen(ctx).GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{
		TenantID: tenantID, TenantSubjectID: payer.UUID(), Currency: cur,
	})
	if err == nil {
		return settingsFromGen(row), nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		d := DefaultAccountSettings(payer)
		d.TenantID = tenantID
		d.Currency = cur
		return d, nil
	}
	return nil, err
}

// AccountSettingsInput is the upsert payload for an payer's spend policy. Only
// non-nil fields are written; nil fields keep their default / existing value.
type AccountSettingsInput struct {
	BillingMode              *string
	MaxSpendPerDayMicros     *int64
	MaxSpendPerMonthMicros   *int64
	MaxOutstandingOwedMicros *int64
	LowBalanceThreshold      *int64
	AutoTopupEnabled         *bool
	AutoTopupAmountCents     *int64
	AutoTopupPaymentMethod   *uuid.UUID
	DefaultCreditExpiryDays  *int
	HardStopOnBreach         *bool
	AlertThresholdPct        *int
}

// UpsertAccountSettings creates or updates the spend policy for (payer,
// credit_type). Validates the billing mode and alert threshold.
func (s *MoneyService) UpsertAccountSettings(ctx context.Context, payer identity.TenantSubjectID, in AccountSettingsInput) (*models.MoneyAccount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Materialize the payable tenant_subjects row so the credit_account_settings
	// FK (migration 076) is satisfied on a subject's first settings write (#317).
	if err := ensureTenantSubject(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID()); err != nil {
		return nil, err
	}
	if in.BillingMode != nil {
		m := strings.ToLower(strings.TrimSpace(*in.BillingMode))
		if m != BillingModePrepaid && m != BillingModeArrears {
			return nil, fmt.Errorf("invalid billing_mode %q", *in.BillingMode)
		}
		in.BillingMode = &m
	}
	if in.AlertThresholdPct != nil && (*in.AlertThresholdPct < 0 || *in.AlertThresholdPct > 100) {
		return nil, fmt.Errorf("alert_threshold_pct must be 0..100")
	}
	tenantID := tid.UUID()
	now := s.now()

	cur, err := s.GetAccountSettings(ctx, payer)
	if err != nil {
		return nil, err
	}
	// Apply overrides onto the current/default view.
	if in.BillingMode != nil {
		cur.BillingMode = *in.BillingMode
	}
	if in.MaxSpendPerDayMicros != nil {
		cur.MaxSpendPerDayMicros = nilIfNeg(in.MaxSpendPerDayMicros)
	}
	if in.MaxSpendPerMonthMicros != nil {
		cur.MaxSpendPerMonthMicros = nilIfNeg(in.MaxSpendPerMonthMicros)
	}
	if in.MaxOutstandingOwedMicros != nil {
		cur.MaxOutstandingOwedMicros = nilIfNeg(in.MaxOutstandingOwedMicros)
	}
	if in.LowBalanceThreshold != nil {
		cur.LowBalanceThreshold = nilIfNeg(in.LowBalanceThreshold)
	}
	if in.AutoTopupEnabled != nil {
		cur.AutoTopupEnabled = *in.AutoTopupEnabled
	}
	if in.AutoTopupAmountCents != nil {
		cur.AutoTopupAmountCents = nilIfNeg(in.AutoTopupAmountCents)
	}
	if in.AutoTopupPaymentMethod != nil {
		cur.AutoTopupPaymentMethod = in.AutoTopupPaymentMethod
	}
	if in.DefaultCreditExpiryDays != nil {
		cur.DefaultCreditExpiryDays = in.DefaultCreditExpiryDays
	}
	if in.HardStopOnBreach != nil {
		cur.HardStopOnBreach = *in.HardStopOnBreach
	}
	if in.AlertThresholdPct != nil {
		cur.AlertThresholdPct = *in.AlertThresholdPct
	}

	cur.TenantID = tenantID
	cur.TenantSubjectID = payer.UUID()
	cur.Currency = normalizeCurrency(cur.Currency)
	// #474 invariant: money_accounts (billing settings) are external-currency-only.
	if err := RequireBillingCurrency(cur.Currency); err != nil {
		return nil, err
	}
	cur.UpdatedAt = now
	if cur.ID == uuid.Nil {
		cur.ID = uuidutil.NewV7()
		cur.CreatedAt = now
	}

	var expiry *int32
	if cur.DefaultCreditExpiryDays != nil {
		v, _ := safecast.Convert[int32](*cur.DefaultCreditExpiryDays)
		expiry = &v
	}
	alertPct, _ := safecast.Convert[int32](cur.AlertThresholdPct)
	if err := s.db.Gen(ctx).UpsertMoneyAccountSettings(ctx, gen.UpsertMoneyAccountSettingsParams{
		ID:                        cur.ID,
		TenantID:                  cur.TenantID,
		TenantSubjectID:           cur.TenantSubjectID,
		Currency:                  cur.Currency,
		BillingMode:               cur.BillingMode,
		MaxSpendPerDayMicros:      cur.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros:    cur.MaxSpendPerMonthMicros,
		MaxOutstandingOwedMicros:  cur.MaxOutstandingOwedMicros,
		LowBalanceThresholdMicros: cur.LowBalanceThreshold,
		AutoTopupEnabled:          cur.AutoTopupEnabled,
		AutoTopupAmountCents:      cur.AutoTopupAmountCents,
		AutoTopupPaymentMethodID:  cur.AutoTopupPaymentMethod,
		DefaultCreditExpiryDays:   expiry,
		HardStopOnBreach:          cur.HardStopOnBreach,
		AlertThresholdPct:         alertPct,
		CreatedAt:                 cur.CreatedAt,
		UpdatedAt:                 cur.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	return s.GetAccountSettings(ctx, payer)
}

func nilIfNeg(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// SetSpendLimit upserts a per-actor sub-limit under (payer, credit_type).
// A nil day/month cap clears that cap. actor is a canonical actor string
// ('serviceToken:<key_id>', 'user:<id>', '<issuer>:<sub>').
func (s *MoneyService) SetSpendLimit(ctx context.Context, payer identity.TenantSubjectID, actor string, maxDay, maxMonth *int64) (*models.MoneySpendLimit, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Materialize the payable tenant_subjects row so the credit_spend_limits FK
	// (migration 076) is satisfied on a subject's first spend-limit write (#317).
	if err := ensureTenantSubject(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID()); err != nil {
		return nil, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, fmt.Errorf("actor required")
	}
	tenantID := tid.UUID()
	now := s.now()
	row := &models.MoneySpendLimit{
		ID:                     uuidutil.NewV7(),
		TenantID:               tenantID,
		TenantSubjectID:        payer.UUID(),
		Currency:               DefaultCurrency,
		Actor:                  actor,
		MaxSpendPerDayMicros:   nilIfNeg(maxDay),
		MaxSpendPerMonthMicros: nilIfNeg(maxMonth),
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	if err := s.db.Gen(ctx).UpsertMoneySpendLimit(ctx, gen.UpsertMoneySpendLimitParams{
		ID:                     row.ID,
		TenantID:               row.TenantID,
		TenantSubjectID:        row.TenantSubjectID,
		Currency:               row.Currency,
		Actor:                  row.Actor,
		MaxSpendPerDayMicros:   row.MaxSpendPerDayMicros,
		MaxSpendPerMonthMicros: row.MaxSpendPerMonthMicros,
		CreatedAt:              row.CreatedAt,
		UpdatedAt:              row.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	return row, nil
}

func (s *MoneyService) getSpendLimit(ctx context.Context, tenantID, payerID uuid.UUID, currency, actor string) (*models.MoneySpendLimit, error) {
	row, err := s.db.Gen(ctx).GetMoneySpendLimit(ctx, gen.GetMoneySpendLimitParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: normalizeCurrency(currency), Actor: actor,
	})
	if err == nil {
		return spendLimitFromGen(row), nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return nil, err
}

// spentInWindow sums spend counted against a rate cap since `since`:
// settled spend (withdrawals + captured holds) PLUS currently-active holds
// created in the window (so concurrent in-flight holds can't overshoot a cap).
// When actor is non-empty the sum is scoped to that actor.
func (s *MoneyService) spentInWindow(ctx context.Context, tenantID, payerID uuid.UUID, currency string, since time.Time, actor string) (int64, error) {
	return s.db.Gen(ctx).SumSpentInMoneyWindow(ctx, gen.SumSpentInMoneyWindowParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: normalizeCurrency(currency),
		Since: since, Actor: actor,
	})
}

// activeHoldsTotal sums all currently-active hold authorizations for an payer
// (current reservation exposure, regardless of window).
func (s *MoneyService) activeHoldsTotal(ctx context.Context, tenantID, payerID uuid.UUID, currency string) (int64, error) {
	return s.db.Gen(ctx).SumActiveMoneyHoldAuthorizations(ctx, gen.SumActiveMoneyHoldAuthorizationsParams{
		TenantID: tenantID, TenantSubjectID: payerID, Currency: normalizeCurrency(currency),
	})
}

// CheckSpendAllowed evaluates the spend policy for (payer, credit_type, actor)
// against an estimated charge, WITHOUT moving money. It enforces, in order:
// per-actor daily/monthly caps, org daily/monthly caps, and the outstanding
// exposure ceiling (settled owed + active holds + this estimate). The balance
// itself is enforced separately by Hold.
func (s *MoneyService) CheckSpendAllowed(ctx context.Context, payer identity.TenantSubjectID, actor string, estimateCents int64) (SpendDecision, error) {
	if s == nil || s.db == nil {
		return SpendDecision{}, fmt.Errorf("money service not initialized")
	}
	cur := DefaultCurrency
	settings, err := s.getAccountSettings(ctx, payer, cur)
	if err != nil {
		return SpendDecision{}, err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return SpendDecision{}, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	now := s.now()
	dayStart, dayReset := dayWindow(now)
	monStart, monReset := monthWindow(now)

	var caps []capInput

	// Per-actor caps (only when a limit row exists for this actor).
	actor = strings.TrimSpace(actor)
	if actor != "" {
		lim, lerr := s.getSpendLimit(ctx, tenantID, payerID, cur, actor)
		if lerr != nil {
			return SpendDecision{}, lerr
		}
		if lim != nil && (lim.MaxSpendPerDayMicros != nil || lim.MaxSpendPerMonthMicros != nil) {
			if lim.MaxSpendPerDayMicros != nil {
				spent, e := s.spentInWindow(ctx, tenantID, payerID, cur, dayStart, actor)
				if e != nil {
					return SpendDecision{}, e
				}
				caps = append(caps, capInput{DenyActorDailyCap, lim.MaxSpendPerDayMicros, spent, &dayReset})
			}
			if lim.MaxSpendPerMonthMicros != nil {
				spent, e := s.spentInWindow(ctx, tenantID, payerID, cur, monStart, actor)
				if e != nil {
					return SpendDecision{}, e
				}
				caps = append(caps, capInput{DenyActorMonthlyCap, lim.MaxSpendPerMonthMicros, spent, &monReset})
			}
		}
	}

	// Tenant-level daily / monthly caps.
	if settings.MaxSpendPerDayMicros != nil {
		spent, e := s.spentInWindow(ctx, tenantID, payerID, cur, dayStart, "")
		if e != nil {
			return SpendDecision{}, e
		}
		caps = append(caps, capInput{DenyDailyCap, settings.MaxSpendPerDayMicros, spent, &dayReset})
	}
	if settings.MaxSpendPerMonthMicros != nil {
		spent, e := s.spentInWindow(ctx, tenantID, payerID, cur, monStart, "")
		if e != nil {
			return SpendDecision{}, e
		}
		caps = append(caps, capInput{DenyMonthlyCap, settings.MaxSpendPerMonthMicros, spent, &monReset})
	}

	// Outstanding exposure ceiling (settled owed + active holds + this estimate).
	if settings.MaxOutstandingOwedMicros != nil {
		held, e := s.activeHoldsTotal(ctx, tenantID, payerID, cur)
		if e != nil {
			return SpendDecision{}, e
		}
		exposure := settings.OutstandingOwedMicros + held
		caps = append(caps, capInput{DenyOutstandingCap, settings.MaxOutstandingOwedMicros, exposure, nil})
	}

	dec := evaluateSpend(caps, estimateCents, settings.HardStopOnBreach, settings.AlertThresholdPct, now)
	return dec, nil
}
