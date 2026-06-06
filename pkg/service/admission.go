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
	TenantSubjectID identity.TenantSubjectID
	InvokerID       string
	Tier            string
	Model           string
	Amounts         map[string]int64
	CreditType      string
	EstimateCents   int64
	Source          string
	SourceID        string
	ExpiresAtUnix   int64
	BlockChecks     []AdmitBlockCheck
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
	Allowed           bool   `json:"allowed"`
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
	if in.TenantSubjectID.IsZero() {
		return nil, fmt.Errorf("tenant_subject_id required")
	}

	lim := ratelimit.NewLimiter(s.rt.RedisClient)
	store := admission.NewTierPolicyStore(s.rt.DB)
	bl := abuse.NewBlocklistService(s.rt.DB)
	bsvc := budgets.NewService(s.rt.DB)
	adm := admission.NewAdmitter(lim, s.creditsService(), store, bl, bsvc)

	var exp time.Time
	switch {
	case in.ExpiresAtUnix > 0:
		exp = time.Unix(in.ExpiresAtUnix, 0).UTC()
	case in.EstimateCents > 0:
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
		TenantSubjectID: in.TenantSubjectID,
		Invoker:         in.InvokerID,
		Tier:            in.Tier,
		Model:           in.Model,
		Amounts:         in.Amounts,
		CreditType:      in.CreditType,
		EstimateCents:   in.EstimateCents,
		Source:          source,
		SourceID:        in.SourceID,
		ExpiresAt:       exp,
		BlockChecks:     checks,
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

// BudgetStatus returns the rolling money-budget windows for (payer, invoker) at a
// tier WITHOUT reserving (issue #304 introspection) — powers a host's /status
// dashboard (e.g. cozy-art useGenerationBudgetStatus).
func (s *Service) BudgetStatus(ctx context.Context, payer identity.TenantSubjectID, invoker, tier string) ([]AdmitBudgetWindowDTO, error) {
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
	statuses, _, err := bsvc.Check(ctx, payer, invoker, pol.BudgetWindows, 0)
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
			Remaining: w.Remaining, ResetAfterSeconds: rs, Allowed: w.Allowed,
		})
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
	Key             string `json:"key"`
	WindowSeconds   int64  `json:"window_seconds"`
	LimitMillicents int64  `json:"limit_millicents"`
}
type TierPolicyInput struct {
	Tier              string                  `json:"tier"`
	Windows           []TierWindowInput       `json:"windows"`
	EntitledEndpoints []string                `json:"entitled_endpoints"`
	BudgetWindows     []TierBudgetWindowInput `json:"budget_windows"`
}

// SetTierPolicy upserts a per-payer tier policy (throughput + entitled endpoints
// + money-budget windows).
func (s *Service) SetTierPolicy(ctx context.Context, payer identity.TenantSubjectID, in TierPolicyInput) error {
	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if in.Tier == "" {
		return fmt.Errorf("tier required")
	}
	pol := models.ThroughputPolicy{EntitledEndpoints: in.EntitledEndpoints}
	for _, w := range in.Windows {
		pol.Windows = append(pol.Windows, models.ThroughputWindow{Unit: w.Unit, WindowSeconds: w.WindowSeconds, Max: w.Max})
	}
	for _, b := range in.BudgetWindows {
		pol.BudgetWindows = append(pol.BudgetWindows, models.BudgetWindowPolicy{Key: b.Key, WindowSeconds: b.WindowSeconds, LimitMillicents: b.LimitMillicents})
	}
	return admission.NewTierPolicyStore(s.rt.DB).UpsertTierPolicyFull(ctx, payer, in.Tier, pol)
}
