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

// overdue is an open invoice still owed past its due date.
func overdue(inv *billing.Invoice, now time.Time) bool {
	return inv.Status == billing.InvoiceOpen && inv.AmountDue > 0 && inv.DueAt != nil && inv.DueAt.Before(now)
}

// markDelinquent marks each overdue invoice whose customer the delinquency
// pass found delinquent in its currency: grace has run out and new usage is
// refused.
func (s *Service) markDelinquent(ctx context.Context, invoices ...*billing.Invoice) error {
	now := s.now()
	var customers []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, inv := range invoices {
		if inv != nil && overdue(inv, now) && !seen[inv.CustomerID.UUID()] {
			seen[inv.CustomerID.UUID()] = true
			customers = append(customers, inv.CustomerID.UUID())
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
	for _, inv := range invoices {
		if inv != nil {
			inv.Delinquent = overdue(inv, now) && delinquent[key{inv.CustomerID.UUID(), inv.Currency}]
		}
	}
	return nil
}
