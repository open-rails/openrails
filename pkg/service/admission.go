package service

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
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
	Owner         identity.OwnerOrgID
	Actor         string
	Tier          string
	Model         string
	Amounts       map[string]int64
	CreditType    string
	EstimateCents int64
	Source        string
	SourceID      string
	ExpiresAtUnix int64
	BlockChecks   []AdmitBlockCheck
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
	if in.Owner.IsZero() {
		return nil, fmt.Errorf("owner required")
	}

	lim := ratelimit.NewLimiter(s.rt.RedisClient)
	store := admission.NewTierPolicyStore(s.rt.DB)
	bl := abuse.NewBlocklistService(s.rt.DB)
	adm := admission.NewAdmitter(lim, s.creditsService(), store, bl)

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
		Owner:         in.Owner,
		Actor:         in.Actor,
		Tier:          in.Tier,
		Model:         in.Model,
		Amounts:       in.Amounts,
		CreditType:    in.CreditType,
		EstimateCents: in.EstimateCents,
		Source:        source,
		SourceID:      in.SourceID,
		ExpiresAt:     exp,
		BlockChecks:   checks,
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
	return res, nil
}
