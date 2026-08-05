package service

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/pkg/identity"
)

// DelinquencyState is the arrears delinquency level for one payer in one
// currency: current | grace | delinquent (or#878).
type DelinquencyState = delinquency.State

// DelinquencySnapshot is one payer's delinquency state, its overdue exposure,
// and when that state began.
type DelinquencySnapshot = delinquency.Snapshot

// DelinquencyPolicy is the merchant's declared grace window and amount floor.
type DelinquencyPolicy = delinquency.Policy

func (s *Service) delinquencyService() *delinquency.Service {
	if s == nil || s.rt == nil || s.rt.DB == nil {
		return nil
	}
	return delinquency.NewService(s.rt.DB, s.rt.Clock)
}

// GetDelinquency returns one payer's delinquency state in every currency it
// owes in. A payer with no row has never been overdue: absence IS `current`,
// and the caller reads an empty slice rather than a fabricated state.
func (s *Service) GetDelinquency(ctx context.Context, payer identity.CustomerID) ([]DelinquencySnapshot, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	svc := s.delinquencyService()
	if svc == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	return svc.ListForCustomer(ctx, payer)
}

// ListDelinquency returns the merchant's overdue roster — payers in grace or
// delinquent, oldest debt first. `state` filters to one level ("" = both).
func (s *Service) ListDelinquency(ctx context.Context, state DelinquencyState, limit int) ([]DelinquencySnapshot, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	svc := s.delinquencyService()
	if svc == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	return svc.List(ctx, state, limit)
}

// GetDelinquencyPolicy returns the merchant's effective delinquency policy —
// the declared values, or the defaults/derivations that stand in for them.
func (s *Service) GetDelinquencyPolicy(ctx context.Context) (DelinquencyPolicy, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return DelinquencyPolicy{}, pinErr
	}
	defer release()

	svc := s.delinquencyService()
	if svc == nil {
		return DelinquencyPolicy{}, fmt.Errorf("service not initialized")
	}
	return svc.Policy(ctx)
}
