package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

// CheckoutRoutingDryRun asks which PSP a checkout WOULD get (or#288). Selector
// mirrors CheckoutPayment.Rail: set it to trace an explicitly named PSP, leave
// it empty to trace what the merchant's routing policy would pick.
type CheckoutRoutingDryRun struct {
	PriceID  string
	Country  string
	Selector string
}

// CheckoutRoutingCandidate is one evaluated candidate. Skip is "" when the
// candidate is eligible, else the class that disqualified it.
type CheckoutRoutingCandidate struct {
	Selector string
	Rail     string
	Skip     string
}

// CheckoutRoutingTrace is a dry run's full decision: what a real session would
// choose, plus the exact trace it would persist on checkout_sessions.
type CheckoutRoutingTrace struct {
	Policy     string
	Rule       *int
	Selected   string
	Rail       string
	Mode       string
	Candidates []CheckoutRoutingCandidate
	Reason     *models.CheckoutRoutingReason
}

// DryRunCheckoutRouting explains routing for a price without creating a
// session. It runs the production decision path, so the answer is what checkout
// would actually do — not a prediction of it.
func (s *Service) DryRunCheckoutRouting(ctx context.Context, in CheckoutRoutingDryRun) (*CheckoutRoutingTrace, error) {
	checkoutSessions, err := s.requireCheckoutSessionService()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.PriceID) == "" {
		return nil, fmt.Errorf("price reference is required")
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: database unavailable")
	}
	var decision *checkout.RoutingDecision
	var mode models.CheckoutSessionMode
	if err := rt.DB.RunInMerchantConn(ctx, func(scopedCtx context.Context) error {
		var runErr error
		decision, mode, runErr = checkoutSessions.DryRunRouting(scopedCtx, in.PriceID, in.Country, in.Selector)
		return runErr
	}); err != nil {
		return nil, fmt.Errorf("dry run checkout routing: %w", err)
	}
	trace := &CheckoutRoutingTrace{
		Policy:     decision.Policy,
		Rule:       decision.Rule,
		Selected:   decision.Selected(),
		Rail:       decision.Target.Rail,
		Mode:       string(mode),
		Candidates: make([]CheckoutRoutingCandidate, 0, len(decision.Candidates)),
		Reason:     decision.Reason(),
	}
	for _, candidate := range decision.Candidates {
		trace.Candidates = append(trace.Candidates, CheckoutRoutingCandidate{
			Selector: candidate.Selector,
			Rail:     candidate.Rail,
			Skip:     candidate.Skip,
		})
	}
	return trace, nil
}
