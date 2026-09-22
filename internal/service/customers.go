package service

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// EnsureCustomer materializes or touches the merchant-scoped customer row.
func (s *Service) EnsureCustomer(ctx context.Context, id uuid.UUID) (*openrails.Customer, error) {
	if id == uuid.Nil {
		return nil, &openrails.StatusError{Status: http.StatusBadRequest, ErrorDetails: openrails.ErrorDetails{
			Type: "invalid_request_error", Code: "invalid_customer_id", Message: "customer id is required"}}
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.rt.DB.Gen(ctx).EnsureCustomer(ctx, gen.EnsureCustomerParams{ID: id, MerchantID: mid.UUID()})
	if err != nil {
		return nil, err
	}
	return &openrails.Customer{ID: (openrails.CustomerID(row.ID)).String(), CreatedAt: row.CreatedAt, LastSeenAt: row.LastSeenAt}, nil
}
