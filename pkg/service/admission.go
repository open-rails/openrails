package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/open-rails/openrails/pkg/identity"
)

// AdmitBlockCheck is one (kind,value) tested against the payment blocklist.
type AdmitBlockCheck struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// AdmitInput is the host's admission request: throughput + money + gates.
type AdmitInput struct {
	CustomerID identity.CustomerID
	Actor      string
	Tier       string
	Resource   string
	// Roles are the immutable role UUIDs the actor holds (#473). Each role with a
	// matching (subject, role) budget policy gates this request's spend. The host
	// reads them from the delegated JWT/permission set.
	Roles          []uuid.UUID
	Amounts        map[string]int64
	EstimateMicros int64
	Source         string
	SourceID       string
	ExpiresAtUnix  int64
	BlockChecks    []AdmitBlockCheck
	// TenantThroughput is the host's per-TENANT fixed-window throughput policy
	// (#404). When set, OpenRails enforces these tenant-scoped windows on the
	// invoke path BEFORE the per-actor policy + any hold, so the host (tensorhub)
	// no longer rate-limits invokes locally. Empty = no tenant throughput limit.
	TenantThroughput []AdmitThroughputWindow
}

// AdmitThroughputWindow is one caller-supplied tenant fixed-window throughput
// limit (#404): at most Max units of Unit per WindowSeconds. Unit defaults to
// "request".
type AdmitThroughputWindow struct {
	Unit          string `json:"unit"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}

// AdmitWindowDTO is a throughput window's state for x-ratelimit-* headers.
type AdmitWindowDTO struct {
	Unit              string `json:"unit"`
	Limit             int64  `json:"limit"`
	Remaining         int64  `json:"remaining"`
	ResetAfterSeconds int64  `json:"reset_after_seconds"`
}

// AdmitResult is the unified admission decision returned to the host.
type AdmitResult struct {
	Allowed           bool             `json:"allowed"`
	BlockedBy         string           `json:"blocked_by,omitempty"`
	BlockedUnit       string           `json:"blocked_unit,omitempty"`
	DenyCode          string           `json:"deny_code,omitempty"`
	RetryAfterSeconds int64            `json:"retry_after_seconds,omitempty"`
	Windows           []AdmitWindowDTO `json:"windows,omitempty"`
	ReservationID     string           `json:"reservation_id,omitempty"`
	// Budget (#304): the rolling money-budget reservation + per-window state.
	BudgetReservationID string                 `json:"budget_reservation_id,omitempty"`
	BudgetWindows       []AdmitBudgetWindowDTO `json:"budget_windows,omitempty"`
	// ResolvedTier is the tier this verdict was evaluated under (#477): explicit
	// req.Tier, else the auto-graduated tier (#476), else the lowest default. The
	// host reads it back to drive its own per-tier capacity decisions (e.g.
	// tensorhub's scheduler in-flight concurrency cap) without a second round-trip.
	ResolvedTier string `json:"resolved_tier,omitempty"`
	// MaxConcurrentHeldMicros is the resolved tier's cap on the sum of the payer's
	// active (un-settled) hold $ (#487; 0 = uncapped). HeldMicros is the payer's
	// active-hold $ sum as evaluated for this verdict (includes the hold just placed
	// on an allow). A host that enforces true occupancy itself reads these to queue
	// against cap + per-job estimate instead of taking the hard deny.
	MaxConcurrentHeldMicros int64 `json:"max_concurrent_held_micros,omitempty"`
	HeldMicros              int64 `json:"held_micros,omitempty"`
	// MaxSingleChargeMicros is the resolved tier's per-charge ceiling (#487; 0 =
	// uncapped). An estimate above it is denied with deny_code
	// "single_charge_cap_exceeded".
	MaxSingleChargeMicros int64 `json:"max_single_charge_micros,omitempty"`
}

// AdmitBudgetWindowDTO is a rolling money-budget window's state (#304), for the
// host's /status dashboard.
type AdmitBudgetWindowDTO struct {
	Key               string `json:"key"`
	Limit             int64  `json:"limit"`
	Used              int64  `json:"used"`
	Reserved          int64  `json:"reserved"`
	Remaining         int64  `json:"remaining"`
	ResetAfterSeconds int64  `json:"reset_after_seconds"`
	// ResetAt is the exact window boundary (#337 fixed windows) — displayable
	// as "your next reset is 4:30pm". Zero when the engine predates state.
	ResetAt time.Time `json:"reset_at,omitzero"`
	Cadence string    `json:"cadence,omitempty"`
	Allowed bool      `json:"allowed"`
}

// Admit runs the unified admission check (issue #298): blocklist + suspension +
// endpoint gating + throughput (Redis) + money (ledger hold). It builds the
// admitter from the runtime (Redis + DB + credits) per call.
func (s *Service) Admit(ctx context.Context, in AdmitInput) (*AdmitResult, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if s.rt.RedisClient == nil {
		return nil, fmt.Errorf("admission unavailable: redis not configured")
	}
	if in.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}

	lim := ratelimit.NewLimiter(s.rt.RedisClient)
	store := admission.NewTierPolicyStore(s.rt.DB)
	bl := abuse.NewBlocklistService(s.rt.DB)
	bsvc := budgets.NewService(s.rt.DB)
	adm := admission.NewAdmitter(lim, s.moneyService(), store, bl, bsvc).
		WithBudgetScopes(admission.NewBudgetPolicyStore(s.rt.DB))

	// #404: tenant-scoped throughput. The host passes its per-tenant RPM/RPD
	// windows; OpenRails enforces them on the invoke path BEFORE the per-actor
	// policy + any credit hold, so a tenant breach places no hold and the host
	// stops rate-limiting invokes locally.
	if len(in.TenantThroughput) > 0 {
		windows := make([]ratelimit.Limit, 0, len(in.TenantThroughput))
		for _, w := range in.TenantThroughput {
			if w.Max <= 0 || w.WindowSeconds <= 0 {
				continue
			}
			unit := w.Unit
			if unit == "" {
				unit = "request"
			}
			windows = append(windows, ratelimit.Limit{Unit: unit, Window: time.Duration(w.WindowSeconds) * time.Second, Max: w.Max})
		}
		if len(windows) > 0 {
			tdec, err := lim.Check(ctx, "tenant:"+in.CustomerID.UUID().String(), ratelimit.Policy{Windows: windows}, map[string]int64{"request": 1})
			if err != nil {
				return nil, err
			}
			if !tdec.Allowed {
				res := &AdmitResult{Allowed: false, BlockedBy: "throughput", BlockedUnit: tdec.BlockedUnit}
				for _, w := range tdec.Windows {
					res.Windows = append(res.Windows, AdmitWindowDTO{Unit: w.Unit, Limit: w.Limit, Remaining: w.Remaining, ResetAfterSeconds: int64(w.ResetAfter / time.Second)})
					if w.Unit == tdec.BlockedUnit {
						res.RetryAfterSeconds = int64(w.ResetAfter / time.Second)
					}
				}
				return res, nil
			}
		}
	}

	var exp time.Time
	switch {
	case in.ExpiresAtUnix > 0:
		exp = time.Unix(in.ExpiresAtUnix, 0).UTC()
	case in.EstimateMicros > 0:
		exp = s.now().Add(time.Hour)
	}
	source := in.Source
	if source == "" {
		source = "admit"
	}

	checks := make([]admission.BlockCheck, 0, len(in.BlockChecks))
	for _, b := range in.BlockChecks {
		checks = append(checks, admission.BlockCheck{Kind: b.Kind, Value: b.Value})
	}

	dec, err := adm.Admit(ctx, admission.AdmitRequest{
		CustomerID:     in.CustomerID,
		Actor:          in.Actor,
		Tier:           in.Tier,
		Resource:       in.Resource,
		Roles:          in.Roles,
		Amounts:        in.Amounts,
		EstimateMicros: in.EstimateMicros,
		Source:         source,
		SourceID:       in.SourceID,
		ExpiresAt:      exp,
		BlockChecks:    checks,
	})
	if err != nil {
		return nil, err
	}

	res := &AdmitResult{
		Allowed:           dec.Allowed,
		BlockedBy:         dec.BlockedBy,
		BlockedUnit:       dec.BlockedUnit,
		DenyCode:          dec.DenyCode,
		RetryAfterSeconds: int64(dec.RetryAfter / time.Second),
		ResolvedTier:      dec.ResolvedTier,

		MaxConcurrentHeldMicros: dec.MaxConcurrentHeldMicros,
		HeldMicros:              dec.HeldMicros,
		MaxSingleChargeMicros:   dec.MaxSingleChargeMicros,
	}
	for _, w := range dec.Windows {
		res.Windows = append(res.Windows, AdmitWindowDTO{
			Unit:              w.Unit,
			Limit:             w.Limit,
			Remaining:         w.Remaining,
			ResetAfterSeconds: int64(w.ResetAfter / time.Second),
		})
	}
	if dec.Hold != nil {
		res.ReservationID = dec.Hold.ID.String()
	}
	if dec.BudgetReservationID != uuid.Nil {
		res.BudgetReservationID = dec.BudgetReservationID.String()
	}
	for _, w := range dec.BudgetWindows {
		rs := int64(time.Until(w.ResetAt) / time.Second)
		if rs < 0 {
			rs = 0
		}
		res.BudgetWindows = append(res.BudgetWindows, AdmitBudgetWindowDTO{
			Key: w.Key, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, ResetAfterSeconds: rs, Allowed: w.Allowed,
		})
	}
	return res, nil
}

// BudgetStatus returns the rolling money-budget windows for (payer, actor) at a
// tier WITHOUT reserving (issue #304 introspection) — powers a host's /status
// dashboard (e.g. cozy-art useGenerationBudgetStatus).
func (s *Service) BudgetStatus(ctx context.Context, payer identity.CustomerID, actor, tier string) ([]AdmitBudgetWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if tier == "" {
		tier = admission.DefaultTier
	}
	store := admission.NewTierPolicyStore(s.rt.DB)
	pol, err := store.GetTierPolicy(ctx, payer, tier)
	if err != nil {
		return nil, err
	}
	bsvc := budgets.NewService(s.rt.DB)
	statuses, _, err := bsvc.Check(ctx, payer, actor, pol.BudgetWindows, 0)
	if err != nil {
		return nil, err
	}
	out := make([]AdmitBudgetWindowDTO, 0, len(statuses))
	for _, w := range statuses {
		rs := int64(time.Until(w.ResetAt) / time.Second)
		if rs < 0 {
			rs = 0
		}
		out = append(out, AdmitBudgetWindowDTO{
			Key: w.Key, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, ResetAfterSeconds: rs, ResetAt: w.ResetAt,
			Cadence: w.Cadence, Allowed: w.Allowed,
		})
	}
	return out, nil
}

// BudgetCheckWindowInput is a caller-supplied fixed budget window for
// BudgetCheck (the host owns the budget policy; OpenRails owns the actuals).
type BudgetCheckWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	LimitMicros   int64  `json:"limit_micros"`
	// Cadence is "session" (default) or "fixed" (#337 fixed windows).
	Cadence string `json:"cadence,omitempty"`
}

// BudgetCheck computes per-window used/reserved/remaining for (payer, actor)
// against CALLER-SUPPLIED windows WITHOUT reserving. Unlike BudgetStatus it does
// NOT read an OpenRails tier policy — the host (tensorhub) owns the platform
// budget policy and passes the windows; OpenRails owns the spend actuals
// (openrails.budget_reservations). Powers the tensorhub delegated budget-window
// display (#410) so it no longer reads tensorhub-local money tables.
func (s *Service) BudgetCheck(ctx context.Context, payer identity.CustomerID, actor string, windows []BudgetCheckWindowInput, requestedMicros int64) ([]AdmitBudgetWindowDTO, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	bw := make([]budgets.BudgetWindow, 0, len(windows))
	for _, w := range windows {
		bw = append(bw, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence})
	}
	bsvc := budgets.NewService(s.rt.DB)
	statuses, _, err := bsvc.Check(ctx, payer, actor, bw, requestedMicros)
	if err != nil {
		return nil, err
	}
	out := make([]AdmitBudgetWindowDTO, 0, len(statuses))
	for _, w := range statuses {
		rs := int64(time.Until(w.ResetAt) / time.Second)
		if rs < 0 {
			rs = 0
		}
		out = append(out, AdmitBudgetWindowDTO{
			Key: w.Key, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, ResetAfterSeconds: rs, ResetAt: w.ResetAt,
			Cadence: w.Cadence, Allowed: w.Allowed,
		})
	}
	return out, nil
}

// BudgetScopeWindowInput is one fixed money-budget window for a budget-scope
// policy (#473): same shape as TierBudgetWindowInput.
type BudgetScopeWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	LimitMicros   int64  `json:"limit_micros"`
	Cadence       string `json:"cadence,omitempty"`
}

// BudgetScopePolicyInput configures one hierarchical budget-scope policy (#473).
// Scope is "subject" | "role" | "actor"; ScopeKey is the role uuid (scope=role)
// or actor string (scope=actor), empty for scope=subject.
type BudgetScopePolicyInput struct {
	Scope    string                   `json:"scope"`
	ScopeKey string                   `json:"scope_key,omitempty"`
	Windows  []BudgetScopeWindowInput `json:"windows"`
}

func budgetScopeWindowModels(ws []BudgetScopeWindowInput) []models.BudgetWindowPolicy {
	out := make([]models.BudgetWindowPolicy, 0, len(ws))
	for _, w := range ws {
		out = append(out, models.BudgetWindowPolicy{Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence})
	}
	return out
}

// SetSubjectBudgetPolicy upserts a SUBJECT-owned budget-scope policy (#473): the
// subject's self (subject) cap, a (subject, role) pool, or a (subject, actor)
// cap. The owner is forced to "subject" — this path can NEVER write a
// platform-owned cap, so a subject cannot create/loosen one.
func (s *Service) SetSubjectBudgetPolicy(ctx context.Context, payer identity.CustomerID, in BudgetScopePolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	return admission.NewBudgetPolicyStore(s.rt.DB).Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: in.Scope, Owner: "subject", ScopeKey: in.ScopeKey,
		Windows: budgetScopeWindowModels(in.Windows),
	})
}

// SetPlatformBudgetPolicy upserts a PLATFORM-owned budget cap (#473) — the
// platform->payer cap (level 1; tensorhub #475 scope B). Callable ONLY from a
// platform-admin path (the caller enforces operator authz before invoking this);
// the subject's own policy surface can never reach it, and the subject's
// SubjectBudgetPolicies read does not expose it.
func (s *Service) SetPlatformBudgetPolicy(ctx context.Context, payer identity.CustomerID, in BudgetScopePolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	return admission.NewBudgetPolicyStore(s.rt.DB).Upsert(ctx, payer, admission.BudgetScopePolicy{
		Scope: in.Scope, Owner: "platform", ScopeKey: in.ScopeKey,
		Windows: budgetScopeWindowModels(in.Windows),
	})
}

// SubjectBudgetPolicies returns ONLY the subject-owned budget-scope policies
// (#473) — platform-owned caps are deliberately invisible to the subject.
func (s *Service) SubjectBudgetPolicies(ctx context.Context, payer identity.CustomerID) ([]BudgetScopePolicyInput, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	rows, err := admission.NewBudgetPolicyStore(s.rt.DB).LoadByOwner(ctx, payer, "subject")
	if err != nil {
		return nil, err
	}
	out := make([]BudgetScopePolicyInput, 0, len(rows))
	for _, r := range rows {
		w := make([]BudgetScopeWindowInput, 0, len(r.Windows))
		for _, ww := range r.Windows {
			w = append(w, BudgetScopeWindowInput{Key: ww.Key, WindowSeconds: ww.WindowSeconds, LimitMicros: ww.LimitMicros, Cadence: ww.Cadence})
		}
		out = append(out, BudgetScopePolicyInput{Scope: r.Scope, ScopeKey: r.ScopeKey, Windows: w})
	}
	return out, nil
}

// TierWindowInput / TierBudgetWindowInput / TierPolicyInput configure a tier's
// policy via the admin endpoint (#298: tier admin API).
type TierWindowInput struct {
	Unit          string `json:"unit"`
	WindowSeconds int64  `json:"window_seconds"`
	Max           int64  `json:"max"`
}
type TierBudgetWindowInput struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	LimitMicros   int64  `json:"limit_micros"`
	// Cadence is "session" (default) or "fixed" (#337 fixed windows).
	Cadence string `json:"cadence,omitempty"`
}

// TierQueueLimitInput is one batch/queue reservation-pool cap (#472 G2): at most
// Max units of Unit in-flight per (payer, endpoint-release). No time window.
type TierQueueLimitInput struct {
	Unit string `json:"unit"`
	Max  int64  `json:"max"`
}
type TierPolicyInput struct {
	Tier              string                  `json:"tier"`
	Windows           []TierWindowInput       `json:"windows"`
	EntitledResources []string                `json:"entitled_resources"`
	BudgetWindows     []TierBudgetWindowInput `json:"budget_windows"`
	// ReleaseWindows holds per-(endpoint-release) throughput window VALUES (#472
	// G1): map key = release uuid, value = that release's window list. Absent
	// releases fall back to Windows.
	ReleaseWindows map[string][]TierWindowInput `json:"release_windows,omitempty"`
	// QueueLimits are the batch/queue reservation-pool caps (#472 G2).
	QueueLimits []TierQueueLimitInput `json:"queue_limits,omitempty"`
	// MaxConcurrentHeldMicros / MaxSingleChargeMicros are the #487 generic
	// $-denominated per-tier admit limits (0 = uncapped): a cap on the payer's
	// active (un-settled) hold $ sum, and a per-charge ceiling.
	MaxConcurrentHeldMicros int64 `json:"max_concurrent_held_micros,omitempty"`
	MaxSingleChargeMicros   int64 `json:"max_single_charge_micros,omitempty"`
}

// SetTierPolicy upserts a per-payer tier policy (throughput + entitled endpoints
// + money-budget windows).
func (s *Service) SetTierPolicy(ctx context.Context, payer identity.CustomerID, in TierPolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	// A ZERO payer writes the TENANT-WIDE DEFAULT tier policy (#477): the platform
	// capacity ladder declared once, applied to every payer at the tier. A non-zero
	// payer writes a per-subject override.
	if in.Tier == "" {
		return fmt.Errorf("tier required")
	}
	pol := models.ThroughputPolicy{
		EntitledResources:       in.EntitledResources,
		MaxConcurrentHeldMicros: in.MaxConcurrentHeldMicros,
		MaxSingleChargeMicros:   in.MaxSingleChargeMicros,
	}
	for _, w := range in.Windows {
		pol.Windows = append(pol.Windows, models.ThroughputWindow{Unit: w.Unit, WindowSeconds: w.WindowSeconds, Max: w.Max})
	}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, LimitMicros: b.LimitMicros, Cadence: b.Cadence})
	}
	if len(in.ReleaseWindows) > 0 {
		pol.ReleaseWindows = make(map[string][]models.ThroughputWindow, len(in.ReleaseWindows))
		for rel, ws := range in.ReleaseWindows {
			out := make([]models.ThroughputWindow, 0, len(ws))
			for _, w := range ws {
				out = append(out, models.ThroughputWindow{Unit: w.Unit, WindowSeconds: w.WindowSeconds, Max: w.Max})
			}
			pol.ReleaseWindows[rel] = out
		}
	}
	for _, q := range in.QueueLimits {
		pol.QueueLimits = append(pol.QueueLimits, models.QueueLimitPolicy{Unit: q.Unit, Max: q.Max})
	}
	return admission.NewTierPolicyStore(s.rt.DB).UpsertTierPolicyFull(ctx, payer, in.Tier, pol)
}

// TierScheduleRung is one rung of a persisted tier ladder (#476): a payer reaches
// Tier once its cumulative paid spend is at least MinCumulativePaidMicros.
type TierScheduleRung struct {
	Tier                    string `json:"tier"`
	MinCumulativePaidMicros int64  `json:"min_cumulative_paid_micros"`
}

// SetTierSchedule persists the tenant's tier SCHEDULE (#476): the host declares
// the ladder ONCE and OpenRails then AUTO-maintains each payer's tier from
// cumulative spend (no host cranking). A zero payer sets the tenant-wide default
// schedule; a non-zero payer sets a per-subject override. owner=platform.
func (s *Service) SetTierSchedule(ctx context.Context, payer identity.CustomerID, schedule []TierScheduleRung) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	moneySvc := money.NewMoneyService(s.rt.DB)
	rungs := make([]money.TierThreshold, 0, len(schedule))
	for _, r := range schedule {
		rungs = append(rungs, money.TierThreshold{Tier: r.Tier, MinPaidMicros: r.MinCumulativePaidMicros})
	}
	return moneySvc.SetTierSchedule(ctx, payer, rungs)
}

// GetTier returns the payer's CURRENT graduated tier (#477): the tier OpenRails
// auto-maintains from cumulative paid spend against the persisted tier schedule
// (#476), or a manual admin override. Empty when the payer has never graduated
// (no tier set) — the caller treats that as the lowest/default tier. This is the
// read the host uses to drive its OWN per-tier capacity decisions (e.g.
// tensorhub's scheduler in-flight concurrency cap) WITHOUT supplying its own
// ladder or cranking graduation.
func (s *Service) GetTier(ctx context.Context, payer identity.CustomerID) (string, error) {
	if s == nil || s.rt == nil {
		return "", fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return "", fmt.Errorf("payer required")
	}
	return money.NewMoneyService(s.rt.DB).GetTier(ctx, payer)
}
