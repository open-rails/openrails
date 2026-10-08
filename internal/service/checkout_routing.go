package service

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// PreviewPSPRouting explains routing for a price without creating a session.
// It runs the production decision path, so the answer is what checkout would
// actually do, not a prediction of it.
func (s *Service) PreviewPSPRouting(ctx context.Context, in billing.PreviewPSPRoutingParams) (*billing.PSPRoutingPreview, error) {
	checkoutAttempts, err := s.requireCheckoutAttemptService()
	if err != nil {
		return nil, err
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	if in.PriceID.IsZero() {
		return nil, apperr.Invalidf("price_id is required").WithParam("price_id")
	}
	var decision *checkout.RoutingDecision
	var mode models.CheckoutAttemptMode
	if err := rt.DB.RunInMerchantConn(ctx, func(scopedCtx context.Context) error {
		var runErr error
		decision, mode, runErr = checkoutAttempts.DryRunRouting(scopedCtx, in.PriceID.String(), "", "", in.Country, in.PSP)
		return runErr
	}); err != nil {
		return nil, apperr.Invalidf("preview PSP routing: %v", err)
	}
	out := &billing.PSPRoutingPreview{
		Policy:     decision.Policy,
		Rule:       decision.Rule,
		Candidates: make([]billing.PSPRoutingCandidate, 0, len(decision.Candidates)),
	}
	if selected := decision.Selected(); selected != "" {
		rail := billing.Rail(decision.Target.Rail)
		modeName := string(mode)
		out.PSP, out.Rail, out.Mode = &selected, &rail, &modeName
	}
	for _, candidate := range decision.Candidates {
		entry := billing.PSPRoutingCandidate{PSP: candidate.Selector, Rail: billing.Rail(candidate.Rail)}
		if candidate.Skip != "" {
			skip := candidate.Skip
			entry.Skip = &skip
		}
		out.Candidates = append(out.Candidates, entry)
	}
	return out, nil
}
