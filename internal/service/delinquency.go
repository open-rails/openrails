package service

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// DelinquencyState is the arrears delinquency level for one payer in one
// currency: current | grace | delinquent (or#878).
type DelinquencyState = delinquency.State

// DelinquencySnapshot is one payer's delinquency state, its overdue exposure,
// and when that state began.
type DelinquencySnapshot = delinquency.Snapshot

func (s *Service) delinquencyService() *delinquency.Service {
	if s == nil || s.rt == nil || s.rt.DB == nil {
		return nil
	}
	return delinquency.NewService(s.rt.DB, s.rt.Clock)
}

func delinquencyFromSnapshot(r DelinquencySnapshot) billing.Delinquency {
	out := billing.Delinquency{
		CustomerID: billing.CustomerID(r.CustomerID), Currency: r.Currency, State: billing.DelinquencyState(r.State),
		OverdueAmount: r.OverdueAmount, OverdueInvoices: r.OverdueInvoices, EnteredAt: r.EnteredAt.UTC(), EvaluatedAt: r.EvaluatedAt.UTC(),
	}
	if r.OverdueSince != nil {
		since := r.OverdueSince.UTC()
		out.OverdueSince = &since
	}
	return out
}

// ListCustomerDelinquency returns a customer's delinquency in every currency
// it has owed in. An empty list means it was never overdue.
func (s *Service) ListCustomerDelinquency(ctx context.Context, customer identity.CustomerID) (billing.ListPage[billing.Delinquency], error) {
	var page billing.ListPage[billing.Delinquency]
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return page, pinErr
	}
	defer release()
	svc := s.delinquencyService()
	if svc == nil {
		return page, fmt.Errorf("service not initialized")
	}
	rows, err := svc.ListForCustomer(ctx, customer)
	if err != nil {
		return page, err
	}
	page.Items = make([]billing.Delinquency, 0, len(rows))
	for _, r := range rows {
		page.Items = append(page.Items, delinquencyFromSnapshot(r))
	}
	return page, nil
}

// ListDelinquency returns the merchant's overdue roster, oldest debt first.
func (s *Service) ListDelinquency(ctx context.Context, params billing.DelinquencyListParams) (billing.ListPage[billing.Delinquency], error) {
	var page billing.ListPage[billing.Delinquency]
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return page, pinErr
	}
	defer release()
	svc := s.delinquencyService()
	if svc == nil {
		return page, fmt.Errorf("service not initialized")
	}
	if params.State != "" && params.State != billing.DelinquencyGrace && params.State != billing.DelinquencyDelinquent {
		return page, apperr.Invalidf("state must be grace or delinquent").WithParam("state")
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return page, err
	}
	var after delinquency.RosterPosition
	present, err := pagination.Decode(params.Cursor, &after)
	if err != nil {
		return page, err
	}
	var from *delinquency.RosterPosition
	if present {
		from = &after
	}
	rows, err := svc.List(ctx, delinquency.State(params.State), from, int(pagination.Fetch(limit)))
	if err != nil {
		return page, err
	}
	items := make([]billing.Delinquency, 0, len(rows))
	for _, r := range rows {
		items = append(items, delinquencyFromSnapshot(r))
	}
	return pagination.Cut(items, limit, func(d billing.Delinquency) any {
		return delinquency.RosterPosition{Since: *d.OverdueSince, Customer: d.CustomerID.UUID(), Currency: d.Currency}
	}), nil
}
