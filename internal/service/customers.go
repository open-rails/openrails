package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	directory "github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ErrCustomerNotFound is a customer the merchant never declared or billed.
var ErrCustomerNotFound = apperr.New(http.StatusNotFound, "customer_not_found", "customer not found")

// errDirectoryUnavailable is a read the merchant's directory could not answer.
var errDirectoryUnavailable = apperr.New(http.StatusServiceUnavailable, "service_unavailable", "The customer directory is unavailable.")

// customers builds the customer objects of rows, in order: their contacts in
// one directory lookup, and each billing section in one read for every row.
func (s *Service) customers(ctx context.Context, mid billing.MerchantID, rows []gen.BillingCustomer) ([]billing.Customer, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	var found map[uuid.UUID]directory.Contact
	if s.rt.Contacts != nil && len(ids) > 0 {
		var err error
		if found, err = s.rt.Contacts.Contacts(ctx, mid, ids); err != nil {
			log.WithContext(ctx).WithError(err).Warn("customer contacts unavailable")
			return nil, errDirectoryUnavailable
		}
	}
	settings, err := customerSettings(ctx, s.rt.DB.Gen(ctx), mid.UUID(), ids)
	if err != nil {
		return nil, err
	}
	money, err := s.moneyService().CustomersMoney(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]billing.Customer, len(rows))
	for i, row := range rows {
		m := money[row.ID]
		out[i] = billing.Customer{
			ID: billing.CustomerID(row.ID), CreatedAt: row.CreatedAt.UTC(), LastSeenAt: row.LastSeenAt.UTC(),
			Settings: settings[row.ID], Balances: m.Balances, CollectionPaymentMethods: m.Collection,
		}
		if contact, ok := found[row.ID]; ok {
			out[i].Contact = customerContact(contact)
		}
	}
	return out, nil
}

func customerContact(c directory.Contact) *billing.CustomerContact {
	text := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	return &billing.CustomerContact{Email: text(c.Email), Name: text(c.Name), Username: text(c.Username), Active: c.Active, SyncedAt: c.SyncedAt}
}

// GetCustomer reads one customer: its contact, its settings, and per currency
// its balance and collection card.
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
	out, err := s.customers(ctx, mid, []gen.BillingCustomer{row})
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

// ListCustomers lists the merchant's customers, newest first, or those its
// directory finds for a search.
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
	q := s.rt.DB.Gen(ctx)
	if params.IDs != nil {
		rows, err := q.ListCustomersByIDs(ctx, gen.ListCustomersByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(params.IDs)})
		if err != nil {
			return page, err
		}
		page.Items, err = s.customers(ctx, mid, rows)
		return page, err
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return page, err
	}
	if search := strings.TrimSpace(params.Search); search != "" {
		ids, err := s.searchCustomers(ctx, mid, search, limit)
		if err != nil {
			return page, err
		}
		rows, err := q.ListCustomersByIDs(ctx, gen.ListCustomersByIDsParams{MerchantID: mid.UUID(), Ids: ids})
		if err != nil {
			return page, err
		}
		page.Items, err = s.customers(ctx, mid, rows[:min(len(rows), limit)])
		return page, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return page, err
	}
	rows, err := q.ListCustomers(ctx, gen.ListCustomersParams{MerchantID: mid.UUID(), AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)})
	if err != nil {
		return page, err
	}
	cut := pagination.Cut(rows, limit, func(c gen.BillingCustomer) any {
		return pagination.TimeID{At: c.CreatedAt, ID: c.ID}
	})
	page.Next = cut.Next
	page.Items, err = s.customers(ctx, mid, cut.Items)
	return page, err
}

// searchCustomers is the ids a search names: the customer whose id it is,
// and those whose contact the directory finds.
func (s *Service) searchCustomers(ctx context.Context, mid billing.MerchantID, search string, limit int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	if id, err := uuid.Parse(search); err == nil {
		ids = append(ids, id)
	}
	if s.rt.Contacts == nil {
		return ids, nil
	}
	found, err := s.rt.Contacts.Search(ctx, mid, search, limit)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("customer search unavailable")
		return nil, errDirectoryUnavailable
	}
	for _, c := range found {
		ids = append(ids, c.CustomerID)
	}
	return ids, nil
}

// GetCustomerAccount is the customer's own summary. A customer never billed
// has no money yet: empty lists, not a refusal.
func (s *Service) GetCustomerAccount(ctx context.Context, customer identity.CustomerID) (*billing.CustomerAccount, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	money, err := s.moneyService().CustomersMoney(ctx, []uuid.UUID{customer.UUID()})
	if err != nil {
		return nil, err
	}
	unread, err := s.rt.DB.Gen(ctx).CountUnreadCustomerNotifications(ctx, gen.CountUnreadCustomerNotificationsParams{MerchantID: mid.UUID(), CustomerID: customer.UUID()})
	if err != nil {
		return nil, err
	}
	m := money[customer.UUID()]
	return &billing.CustomerAccount{ID: billing.CustomerID(customer), Balances: m.Balances, CollectionPaymentMethods: m.Collection, UnreadNotifications: unread}, nil
}
