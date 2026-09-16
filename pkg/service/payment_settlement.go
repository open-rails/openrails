package service

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

func (s *Service) HasSettledPayment(ctx context.Context, customerID, priceID uuid.UUID) (bool, error) {
	if customerID == uuid.Nil || priceID == uuid.Nil {
		return false, &openrails.StatusError{Status: http.StatusBadRequest, ErrorDetails: openrails.ErrorDetails{
			Type: "invalid_request_error", Code: "invalid_settlement_status_request", Message: "customer and price ids are required"}}
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return s.rt.DB.Gen(ctx).HasSettledPayment(ctx, gen.HasSettledPaymentParams{MerchantID: mid.UUID(), CustomerID: customerID, PriceID: priceID})
}
