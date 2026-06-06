package credits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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
	DenyInvokerDailyCap   = "invoker_daily_cap_exceeded"
	DenyInvokerMonthlyCap = "invoker_monthly_cap_exceeded"
	DenyDailyCap          = "daily_cap_exceeded"
	DenyMonthlyCap        = "monthly_cap_exceeded"
	DenyOutstandingCap    = "outstanding_cap_exceeded"
)

// CapResult is the evaluation of a single cap for a spend request.
type CapResult struct {
	Code           string     `json:"code"`
	Limit          int64      `json:"limit_cents"`
	Spent          int64      `json:"spent_cents"`     // spend already counted in the window/exposure
	Projected      int64      `json:"projected_cents"` // spent + estimate
	Remaining      int64      `json:"remaining_cents"` // limit - spent (may be negative)
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
func DefaultAccountSettings(payer identity.TenantSubjectID, creditTypeID uuid.UUID) *models.CreditAccountSettings {
	days := 365
	return &models.CreditAccountSettings{
		TenantSubjectID:         payer.UUID(),
		CreditTypeID:            creditTypeID,
		BillingMode:             BillingModePrepaid,
		HardStopOnBreach:        true,
		AlertThresholdPct:       80,
		DefaultCreditExpiryDays: &days,
	}
}

// GetAccountSettings returns the stored settings for (payer, credit_type), or a
// DefaultAccountSettings value when none exists (never nil, never an error for a
// missing row).
func (s *CreditsService) GetAccountSettings(ctx context.Context, payer identity.TenantSubjectID, creditType string) (*models.CreditAccountSettings, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	out := new(models.CreditAccountSettings)
	err = s.db.Q(ctx).NewSelect().Model(out).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, payer.UUID(), ct.ID).
		Limit(1).Scan(ctx)
	if err == nil {
		return out, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		d := DefaultAccountSettings(payer, ct.ID)
		d.TenantID = tenantID
		return d, nil
	}
	return nil, err
}

// AccountSettingsInput is the upsert payload for an payer's spend policy. Only
// non-nil fields are written; nil fields keep their default / existing value.
type AccountSettingsInput struct {
	BillingMode             *string
	MaxSpendPerDayCents     *int64
	MaxSpendPerMonthCents   *int64
	MaxOutstandingOwedCents *int64
	LowBalanceThreshold     *int64
	AutoTopupEnabled        *bool
	AutoTopupAmountCents    *int64
	AutoTopupPaymentMethod  *uuid.UUID
	DefaultCreditExpiryDays *int
	HardStopOnBreach        *bool
	AlertThresholdPct       *int
}

// UpsertAccountSettings creates or updates the spend policy for (payer,
// credit_type). Validates the billing mode and alert threshold.
func (s *CreditsService) UpsertAccountSettings(ctx context.Context, payer identity.TenantSubjectID, creditType string, in AccountSettingsInput) (*models.CreditAccountSettings, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
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
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	now := s.now()

	cur, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return nil, err
	}
	// Apply overrides onto the current/default view.
	if in.BillingMode != nil {
		cur.BillingMode = *in.BillingMode
	}
	if in.MaxSpendPerDayCents != nil {
		cur.MaxSpendPerDayCents = nilIfNeg(in.MaxSpendPerDayCents)
	}
	if in.MaxSpendPerMonthCents != nil {
		cur.MaxSpendPerMonthCents = nilIfNeg(in.MaxSpendPerMonthCents)
	}
	if in.MaxOutstandingOwedCents != nil {
		cur.MaxOutstandingOwedCents = nilIfNeg(in.MaxOutstandingOwedCents)
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
	cur.CreditTypeID = ct.ID
	cur.UpdatedAt = now
	if cur.ID == uuid.Nil {
		cur.ID = uuidutil.NewV7()
		cur.CreatedAt = now
	}

	_, err = s.db.Q(ctx).NewInsert().Model(cur).
		On("CONFLICT (tenant_id, tenant_subject_id, credit_type_id) DO UPDATE").
		Set("billing_mode = EXCLUDED.billing_mode").
		Set("max_spend_per_day_cents = EXCLUDED.max_spend_per_day_cents").
		Set("max_spend_per_month_cents = EXCLUDED.max_spend_per_month_cents").
		Set("max_outstanding_owed_cents = EXCLUDED.max_outstanding_owed_cents").
		Set("low_balance_threshold_cents = EXCLUDED.low_balance_threshold_cents").
		Set("auto_topup_enabled = EXCLUDED.auto_topup_enabled").
		Set("auto_topup_amount_cents = EXCLUDED.auto_topup_amount_cents").
		Set("auto_topup_payment_method_id = EXCLUDED.auto_topup_payment_method_id").
		Set("default_credit_expiry_days = EXCLUDED.default_credit_expiry_days").
		Set("hard_stop_on_breach = EXCLUDED.hard_stop_on_breach").
		Set("alert_threshold_pct = EXCLUDED.alert_threshold_pct").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	return s.GetAccountSettings(ctx, payer, creditType)
}

func nilIfNeg(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}

// SetSpendLimit upserts a per-invoker sub-limit under (payer, credit_type).
// A nil day/month cap clears that cap. invoker is a canonical invoker string
// ('oat:<key_id>', 'user:<id>', '<issuer>:<sub>').
func (s *CreditsService) SetSpendLimit(ctx context.Context, payer identity.TenantSubjectID, creditType, invoker string, maxDay, maxMonth *int64) (*models.CreditSpendLimit, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("credits service not initialized")
	}
	invoker = strings.TrimSpace(invoker)
	if invoker == "" {
		return nil, fmt.Errorf("invoker required")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return nil, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	now := s.now()
	row := &models.CreditSpendLimit{
		ID:                    uuidutil.NewV7(),
		TenantID:              tenantID,
		TenantSubjectID:       payer.UUID(),
		CreditTypeID:          ct.ID,
		InvokerID:             invoker,
		MaxSpendPerDayCents:   nilIfNeg(maxDay),
		MaxSpendPerMonthCents: nilIfNeg(maxMonth),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	_, err = s.db.Q(ctx).NewInsert().Model(row).
		On("CONFLICT (tenant_id, tenant_subject_id, credit_type_id, invoker_id) DO UPDATE").
		Set("max_spend_per_day_cents = EXCLUDED.max_spend_per_day_cents").
		Set("max_spend_per_month_cents = EXCLUDED.max_spend_per_month_cents").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (s *CreditsService) getSpendLimit(ctx context.Context, tenantID, ownerID, creditTypeID uuid.UUID, invoker string) (*models.CreditSpendLimit, error) {
	row := new(models.CreditSpendLimit)
	err := s.db.Q(ctx).NewSelect().Model(row).
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ? AND invoker_id = ?", tenantID, ownerID, creditTypeID, invoker).
		Limit(1).Scan(ctx)
	if err == nil {
		return row, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return nil, err
}

// spentInWindow sums spend counted against a rate cap since `since`:
// settled spend (withdrawals + captured holds) PLUS currently-active holds
// created in the window (so concurrent in-flight holds can't overshoot a cap).
// When invoker is non-empty the sum is scoped to that invoker.
func (s *CreditsService) spentInWindow(ctx context.Context, tenantID, ownerID, creditTypeID uuid.UUID, since time.Time, invoker string) (int64, error) {
	q := s.db.Q(ctx).NewSelect().
		Model((*models.CreditTransaction)(nil)).
		ColumnExpr("COALESCE(SUM("+
			"CASE "+
			"WHEN transaction_type = 'withdrawal' THEN -amount "+
			"WHEN transaction_type = 'hold' AND status = 'captured' THEN -amount "+
			"WHEN transaction_type = 'hold' AND status = 'active' THEN COALESCE(authorized_amount, 0) "+
			"ELSE 0 END), 0)").
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, ownerID, creditTypeID).
		Where("created_at >= ?", since)
	if invoker != "" {
		q = q.Where("invoker_id = ?", invoker)
	}
	var total int64
	if err := q.Scan(ctx, &total); err != nil {
		return 0, err
	}
	return total, nil
}

// activeHoldsTotal sums all currently-active hold authorizations for an payer
// (current reservation exposure, regardless of window).
func (s *CreditsService) activeHoldsTotal(ctx context.Context, tenantID, ownerID, creditTypeID uuid.UUID) (int64, error) {
	var total int64
	err := s.db.Q(ctx).NewSelect().
		Model((*models.CreditTransaction)(nil)).
		ColumnExpr("COALESCE(SUM(COALESCE(authorized_amount, 0)), 0)").
		Where("tenant_id = ? AND tenant_subject_id = ? AND credit_type_id = ?", tenantID, ownerID, creditTypeID).
		Where("transaction_type = 'hold' AND status = 'active'").
		Scan(ctx, &total)
	return total, err
}

// CheckSpendAllowed evaluates the spend policy for (payer, credit_type, invoker)
// against an estimated charge, WITHOUT moving money. It enforces, in order:
// per-invoker daily/monthly caps, org daily/monthly caps, and the outstanding
// exposure ceiling (settled owed + active holds + this estimate). The balance
// itself is enforced separately by Hold.
func (s *CreditsService) CheckSpendAllowed(ctx context.Context, payer identity.TenantSubjectID, creditType, invoker string, estimateCents int64) (SpendDecision, error) {
	if s == nil || s.db == nil {
		return SpendDecision{}, fmt.Errorf("credits service not initialized")
	}
	ct, err := s.GetCreditTypeByName(ctx, creditType)
	if err != nil {
		return SpendDecision{}, err
	}
	settings, err := s.GetAccountSettings(ctx, payer, creditType)
	if err != nil {
		return SpendDecision{}, err
	}
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()
	now := s.now()
	dayStart, dayReset := dayWindow(now)
	monStart, monReset := monthWindow(now)

	var caps []capInput

	// Per-invoker caps (only when a limit row exists for this invoker).
	invoker = strings.TrimSpace(invoker)
	if invoker != "" {
		lim, lerr := s.getSpendLimit(ctx, tenantID, ownerID, ct.ID, invoker)
		if lerr != nil {
			return SpendDecision{}, lerr
		}
		if lim != nil && (lim.MaxSpendPerDayCents != nil || lim.MaxSpendPerMonthCents != nil) {
			if lim.MaxSpendPerDayCents != nil {
				spent, e := s.spentInWindow(ctx, tenantID, ownerID, ct.ID, dayStart, invoker)
				if e != nil {
					return SpendDecision{}, e
				}
				caps = append(caps, capInput{DenyInvokerDailyCap, lim.MaxSpendPerDayCents, spent, &dayReset})
			}
			if lim.MaxSpendPerMonthCents != nil {
				spent, e := s.spentInWindow(ctx, tenantID, ownerID, ct.ID, monStart, invoker)
				if e != nil {
					return SpendDecision{}, e
				}
				caps = append(caps, capInput{DenyInvokerMonthlyCap, lim.MaxSpendPerMonthCents, spent, &monReset})
			}
		}
	}

	// Org-level daily / monthly caps.
	if settings.MaxSpendPerDayCents != nil {
		spent, e := s.spentInWindow(ctx, tenantID, ownerID, ct.ID, dayStart, "")
		if e != nil {
			return SpendDecision{}, e
		}
		caps = append(caps, capInput{DenyDailyCap, settings.MaxSpendPerDayCents, spent, &dayReset})
	}
	if settings.MaxSpendPerMonthCents != nil {
		spent, e := s.spentInWindow(ctx, tenantID, ownerID, ct.ID, monStart, "")
		if e != nil {
			return SpendDecision{}, e
		}
		caps = append(caps, capInput{DenyMonthlyCap, settings.MaxSpendPerMonthCents, spent, &monReset})
	}

	// Outstanding exposure ceiling (settled owed + active holds + this estimate).
	if settings.MaxOutstandingOwedCents != nil {
		held, e := s.activeHoldsTotal(ctx, tenantID, ownerID, ct.ID)
		if e != nil {
			return SpendDecision{}, e
		}
		exposure := settings.OutstandingOwedCents + held
		caps = append(caps, capInput{DenyOutstandingCap, settings.MaxOutstandingOwedCents, exposure, nil})
	}

	dec := evaluateSpend(caps, estimateCents, settings.HardStopOnBreach, settings.AlertThresholdPct, now)
	return dec, nil
}
