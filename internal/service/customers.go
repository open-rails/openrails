package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ErrCustomerNotFound is a customer the merchant never declared or billed.
var ErrCustomerNotFound = apperr.New(http.StatusNotFound, "customer_not_found", "customer not found")

func customerFromRow(row gen.BillingCustomer) billing.Customer {
	return billing.Customer{ID: billing.CustomerID(row.ID), Email: row.Email, Username: row.Username, Blocked: row.Blocked, CreatedAt: row.CreatedAt, LastSeenAt: row.LastSeenAt}
}

// EnsureCustomers creates the merchant's customers or replaces their
// declared fields, in one statement: every item or none. Items name distinct
// customers.
func (s *Service) EnsureCustomers(ctx context.Context, items []billing.EnsureCustomerParams) ([]billing.Customer, error) {
	if len(items) == 0 || len(items) > billing.MaxBatchItems {
		return nil, apperr.Invalidf("items must hold 1 to %d customers", billing.MaxBatchItems).WithParam("items")
	}
	ids := make([]uuid.UUID, len(items))
	emails := make([]string, len(items))
	usernames := make([]string, len(items))
	blocked := make([]bool, len(items))
	seen := make(map[billing.CustomerID]bool, len(items))
	for i, item := range items {
		if item.ID.IsZero() {
			return nil, apperr.Invalidf("customer id is required").WithParam(apperr.ItemParam(i, "id"))
		}
		if seen[item.ID] {
			return nil, apperr.Invalidf("customer %s is declared twice", item.ID).WithParam(apperr.ItemParam(i, "id"))
		}
		seen[item.ID] = true
		if item.Email != nil {
			email := strings.TrimSpace(*item.Email)
			if email == "" || len(email) > 320 || !strings.Contains(email, "@") {
				return nil, apperr.Invalidf("email must be an address of at most 320 bytes").WithParam(apperr.ItemParam(i, "email"))
			}
			emails[i] = email
		}
		if item.Username != nil {
			username := strings.TrimSpace(*item.Username)
			if username == "" || len(username) > 256 {
				return nil, apperr.Invalidf("username must be at most 256 bytes and not blank").WithParam(apperr.ItemParam(i, "username"))
			}
			usernames[i] = username
		}
		ids[i], blocked[i] = item.ID.UUID(), item.Blocked
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
	rows, err := s.rt.DB.Gen(ctx).PutCustomers(ctx, gen.PutCustomersParams{MerchantID: mid.UUID(), Ids: ids, Emails: emails, Usernames: usernames, Blocked: blocked})
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]billing.Customer, len(rows))
	for _, row := range rows {
		byID[row.ID] = customerFromRow(row)
	}
	out := make([]billing.Customer, len(ids))
	for i, id := range ids {
		out[i] = byID[id]
	}
	return out, nil
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
	if params.IDs != nil {
		rows, err := s.rt.DB.Gen(ctx).ListCustomersByIDs(ctx, gen.ListCustomersByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(params.IDs)})
		if err != nil {
			return page, err
		}
		for _, row := range rows {
			page.Items = append(page.Items, customerFromRow(row))
		}
		return page, nil
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
