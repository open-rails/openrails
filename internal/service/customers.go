package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrCustomerNotFound is a customer the merchant never declared or billed.
var ErrCustomerNotFound = apperr.New(http.StatusNotFound, "customer_not_found", "customer not found")

func customerFromRow(row gen.BillingCustomer) billing.Customer {
	return billing.Customer{ID: billing.CustomerID(row.ID), Email: row.Email, Username: row.Username, Blocked: row.Blocked, CreatedAt: row.CreatedAt, LastSeenAt: row.LastSeenAt}
}

// EnsureCustomer creates the merchant's customer or replaces its declared
// fields.
func (s *Service) EnsureCustomer(ctx context.Context, id identity.CustomerID, params billing.EnsureCustomerParams) (*billing.Customer, error) {
	if id.IsZero() {
		return nil, apperr.Invalidf("customer id is required").WithParam("customer_id")
	}
	if params.Email != nil {
		email := strings.TrimSpace(*params.Email)
		if email == "" || len(email) > 320 || !strings.Contains(email, "@") {
			return nil, apperr.Invalidf("email must be an address of at most 320 bytes").WithParam("email")
		}
		params.Email = &email
	}
	if params.Username != nil {
		username := strings.TrimSpace(*params.Username)
		if username == "" || len(username) > 256 {
			return nil, apperr.Invalidf("username must be at most 256 bytes and not blank").WithParam("username")
		}
		params.Username = &username
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
	row, err := s.rt.DB.Gen(ctx).PutCustomer(ctx, gen.PutCustomerParams{ID: id.UUID(), MerchantID: mid.UUID(), Email: params.Email, Username: params.Username, Blocked: params.Blocked})
	if err != nil {
		return nil, err
	}
	out := customerFromRow(row)
	return &out, nil
}

// GetCustomer reads one customer.
func (s *Service) GetCustomer(ctx context.Context, id identity.CustomerID) (*billing.Customer, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.rt.DB.Gen(ctx).GetCustomer(ctx, gen.GetCustomerParams{MerchantID: mid.UUID(), ID: id.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCustomerNotFound
	}
	if err != nil {
		return nil, err
	}
	out := customerFromRow(row)
	return &out, nil
}

// ListCustomers lists the merchant's customers, newest first.
func (s *Service) ListCustomers(ctx context.Context, params billing.CustomerListParams) (billing.ListPage[billing.Customer], error) {
	var page billing.ListPage[billing.Customer]
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return page, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return page, err
	}
	rows, err := s.rt.DB.Gen(ctx).ListCustomers(ctx, gen.ListCustomersParams{
		MerchantID: mid.UUID(), Q: strings.TrimSpace(params.Query), AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit),
	})
	if err != nil {
		return page, err
	}
	customers := make([]billing.Customer, 0, len(rows))
	for _, row := range rows {
		customers = append(customers, customerFromRow(row))
	}
	return pagination.Cut(customers, limit, func(c billing.Customer) any {
		return pagination.TimeID{At: c.CreatedAt, ID: c.ID.UUID()}
	}), nil
}
