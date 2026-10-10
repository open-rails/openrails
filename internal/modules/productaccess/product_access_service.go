// Package productaccess grants products for free and reads the products
// customers hold. A product's keys follow it: a granted holder derives the
// product's current keys like a buyer.
package productaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// ErrProductNotFound: a grant names no product of the merchant.
var ErrProductNotFound = apperr.New(http.StatusNotFound, "resource_not_found", "product not found")

// ErrIdempotencyKeyReused: the key already granted another customer or product.
var ErrIdempotencyKeyReused = apperr.New(http.StatusUnprocessableEntity, "idempotency_key_reused", "idempotency key already granted another product or customer")

// Service owns free product grants and product access reads.
type Service struct {
	db    *db.DB
	clock clockwork.Clock
}

func NewService(database *db.DB, clocks ...clockwork.Clock) *Service {
	return &Service{db: database, clock: timeutil.FirstClock(clocks...)}
}

// SetClock overrides the service clock (tests).
func (s *Service) SetClock(c clockwork.Clock) { s.clock = timeutil.FirstClock(c) }

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// Grant is one free product grant. Exactly one of EndsAt and Hours, or
// neither for an indefinite grant. Hours extends after the customer's latest
// live window of the product.
type Grant struct {
	CustomerID     uuid.UUID
	ProductID      uuid.UUID
	StartsAt       *time.Time
	EndsAt         *time.Time
	Hours          *int
	Reason         grants.GrantReason
	Note           *string
	IdempotencyKey string
	Actor          string
}

// GrantProduct records one free grant once per idempotency key. A replay
// returns the recorded window and created=false.
func (s *Service) GrantProduct(ctx context.Context, g Grant) (*models.ProductAccess, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("product access service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	recorded, err := s.db.Gen(ctx).GetAccessGrantByIdempotencyKey(ctx, gen.GetAccessGrantByIdempotencyKeyParams{MerchantID: mid.UUID(), IdempotencyKey: g.IdempotencyKey})
	if err == nil {
		if recorded.CustomerID != g.CustomerID || recorded.ProductID == nil || *recorded.ProductID != g.ProductID {
			return nil, false, ErrIdempotencyKeyReused
		}
		window, err := s.db.Gen(ctx).GetProductAccessByGrant(ctx, gen.GetProductAccessByGrantParams{MerchantID: mid.UUID(), GrantID: recorded.ID})
		if err != nil {
			return nil, false, err
		}
		return models.ProductAccessFromGen(window), false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if _, err := s.db.Gen(ctx).GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: mid.UUID(), ID: g.ProductID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrProductNotFound
		}
		return nil, false, err
	}
	if err := db.EnsureCustomerRowQ(ctx, s.db.Gen(ctx), mid.UUID(), g.CustomerID); err != nil {
		return nil, false, err
	}
	p := entitlements.PushAccessParams{
		UserID: g.CustomerID.String(), CustomerID: g.CustomerID, ProductID: g.ProductID, NotBefore: g.StartsAt,
		SourceType: models.AccessSourceGrant, SourceID: g.IdempotencyKey,
		Actor: g.Actor, GrantReason: g.Reason, Note: g.Note,
	}
	switch {
	case g.Hours != nil:
		d := time.Duration(*g.Hours) * time.Hour
		p.Duration = &d
	case g.EndsAt != nil:
		p.EndsAt = g.EndsAt
	default:
		p.Indefinite = true
	}
	ents := entitlements.NewEntitlementService(s.db, s.clock)
	window, err := ents.PushAccess(ctx, p)
	if err != nil {
		return nil, false, err
	}
	if window == nil || window.SourceID != g.IdempotencyKey || window.CustomerID != g.CustomerID || window.ProductID != g.ProductID {
		return nil, false, ErrIdempotencyKeyReused
	}
	return window, true, nil
}

// RevokeProductAccess revokes one of the customer's windows. Not found or
// already revoked reports found=false so callers stay idempotent.
func (s *Service) RevokeProductAccess(ctx context.Context, customer, accessID uuid.UUID, reason models.AccessRevokeReason) (found bool, err error) {
	ents := entitlements.NewEntitlementService(s.db, s.clock)
	window, err := ents.GetAccessByID(ctx, accessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if window.CustomerID != customer || window.RevokedAt != nil {
		return false, nil
	}
	return true, ents.RevokeGrantedAccess(ctx, window, reason)
}

// CheckProducts answers, for each product, whether the customer holds it now.
func (s *Service) CheckProducts(ctx context.Context, userID string, products []uuid.UUID) (map[uuid.UUID]bool, error) {
	if len(products) > 100 {
		return nil, errors.New("at most 100 product IDs are allowed")
	}
	for _, id := range products {
		if id == uuid.Nil {
			return nil, errors.New("product ID is required")
		}
	}
	result := make(map[uuid.UUID]bool, len(products))
	if len(products) == 0 {
		return result, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	customer, err := db.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).CheckProductAccess(ctx, gen.CheckProductAccessParams{MerchantID: mid.UUID(), CustomerID: customer, ProductIds: products, AtTime: s.now().UTC()})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ProductID] = row.HasAccess
	}
	return result, nil
}

// HasPermanentAccess reports whether the customer holds the product
// indefinitely now.
func (s *Service) HasPermanentAccess(ctx context.Context, customer, product uuid.UUID) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return s.db.Gen(ctx).HasPermanentProductAccess(ctx, gen.HasPermanentProductAccessParams{MerchantID: mid.UUID(), CustomerID: customer, ProductID: product, AtTime: s.now().UTC()})
}

// ListPage returns one page of the customer's windows, newest first: those
// live now when liveOnly, else every window that was not removed. more:
// another page follows.
func (s *Service) ListPage(ctx context.Context, customer uuid.UUID, after *uuid.UUID, limit int, liveOnly bool) ([]gen.ListProductAccessPageRow, bool, error) {
	if limit < 1 || limit > 500 {
		return nil, false, errors.New("limit must be between 1 and 500")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	rows, err := s.db.Gen(ctx).ListProductAccessPage(ctx, gen.ListProductAccessPageParams{
		MerchantID: mid.UUID(), CustomerID: customer, LiveOnly: liveOnly, AtTime: s.now().UTC(), AfterID: after, FetchLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// ListByIDs reads a customer's named windows, newest first.
func (s *Service) ListByIDs(ctx context.Context, customer uuid.UUID, ids []uuid.UUID) ([]gen.ListProductAccessPageRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListCustomerProductAccessByIDs(ctx, gen.ListCustomerProductAccessByIDsParams{MerchantID: mid.UUID(), CustomerID: customer, Ids: ids})
	if err != nil {
		return nil, err
	}
	out := make([]gen.ListProductAccessPageRow, len(rows))
	for i, row := range rows {
		out[i] = gen.ListProductAccessPageRow(row)
	}
	return out, nil
}

// GrantProducts records free grants all or none, in one transaction, and
// returns each window with its product and grant attribution, in order.
func (s *Service) GrantProducts(ctx context.Context, batch []Grant) ([]gen.ListProductAccessPageRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("product access service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out []gen.ListProductAccessPageRow
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound := NewService(s.db.NewWithPgxTx(tx), s.clock)
		ids := make([]uuid.UUID, len(batch))
		for i, g := range batch {
			window, _, err := bound.GrantProduct(ctx, g)
			if err != nil {
				return err
			}
			ids[i] = window.ID
		}
		views, err := gen.New(tx).ListProductAccessViews(ctx, gen.ListProductAccessViewsParams{MerchantID: mid.UUID(), Ids: ids})
		if err != nil {
			return err
		}
		byID := make(map[uuid.UUID]gen.ListProductAccessPageRow, len(views))
		for _, v := range views {
			byID[v.ID] = gen.ListProductAccessPageRow(v)
		}
		out = make([]gen.ListProductAccessPageRow, len(ids))
		for i, id := range ids {
			view, ok := byID[id]
			if !ok {
				return fmt.Errorf("granted window %s vanished", id)
			}
			out[i] = view
		}
		return nil
	})
	return out, err
}
