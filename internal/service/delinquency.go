package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/delinquency"
)

func (s *Service) delinquencyService() *delinquency.Service {
	if s == nil || s.rt == nil || s.rt.DB == nil {
		return nil
	}
	return delinquency.NewService(s.rt.DB, s.rt.Clock)
}

// overdue is a usage order still owed past its due date.
func overdue(o *billing.Order, now time.Time) bool {
	return o.Usage != nil && o.Status == billing.OrderOpen && o.Usage.AmountDue > 0 && o.Usage.DueAt.Before(now)
}

// markDelinquent marks each overdue usage order whose customer the
// delinquency pass found delinquent in its currency: grace has run out and
// new usage is refused.
func (s *Service) markDelinquent(ctx context.Context, views ...*billing.Order) error {
	now := s.now()
	var customers []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, o := range views {
		if o != nil && overdue(o, now) && !seen[o.CustomerID.UUID()] {
			seen[o.CustomerID.UUID()] = true
			customers = append(customers, o.CustomerID.UUID())
		}
	}
	if len(customers) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	rows, err := s.rt.DB.Gen(ctx).ListCustomersDelinquency(ctx, gen.ListCustomersDelinquencyParams{
		MerchantID: mid.UUID(), CustomerIds: customers, RowLimit: int32(len(customers) * len(billing.Currencies())), // #nosec G115 -- customers is at most one page (MaxPageLimit), times the currency registry
	})
	if err != nil {
		return err
	}
	type key struct {
		customer uuid.UUID
		currency string
	}
	delinquent := map[key]bool{}
	for _, row := range rows {
		if delinquency.State(row.State) == delinquency.StateDelinquent {
			delinquent[key{row.CustomerID, row.Currency}] = true
		}
	}
	for _, o := range views {
		if o != nil && overdue(o, now) {
			o.Usage.Delinquent = delinquent[key{o.CustomerID.UUID(), o.Currency}]
		}
	}
	return nil
}

// markDelinquentPage marks a page of orders.
func (s *Service) markDelinquentPage(ctx context.Context, page []billing.Order) error {
	views := make([]*billing.Order, len(page))
	for i := range page {
		views[i] = &page[i]
	}
	return s.markDelinquent(ctx, views...)
}
