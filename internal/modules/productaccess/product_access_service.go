// Package productaccess grants products for free and reads the products
// customers hold. A product's keys follow it: a granted holder derives the
// product's current keys like a buyer.
package productaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/destructive"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/convergejob"
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
	destructiveAdmissions *destructive.Admissions
	db                    *db.DB
	clock                 clockwork.Clock
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

// GrantProduct records one free grant once per idempotency key, with its
// customer's convergence. A replay returns the recorded window and
// created=false.
func (s *Service) GrantProduct(ctx context.Context, g Grant) (*models.ProductAccess, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("product access service not initialized")
	}
	if s.db.Tx() == nil {
		var window *models.ProductAccess
		var created bool
		err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			window, created, err = NewService(s.db.NewWithPgxTx(tx), s.clock).GrantProduct(ctx, g)
			return err
		})
		return window, created, err
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
	return window, true, convergejob.Queue(ctx, s.db, mid, g.CustomerID)
}

// RevokeProductAccess revokes a window; a revoked window stays as it is. Not
// found reports found=false.
func (s *Service) RevokeProductAccess(ctx context.Context, accessID uuid.UUID, reason models.AccessRevokeReason) (bool, error) {
	return s.RevokeProductAccessAsOf(ctx, accessID, reason, nil)
}

// RevokeProductAccessAsOf is the operator finding's explicit effective-time
// revocation. Its admission time always comes from the command clock.
func (s *Service) RevokeProductAccessAsOf(ctx context.Context, accessID uuid.UUID, reason models.AccessRevokeReason, asOf *time.Time) (bool, error) {
	ents := entitlements.NewEntitlementService(s.db, s.clock)
	window, err := ents.GetAccessByID(ctx, accessID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if window.RevokedAt != nil || window.DeletedAt != nil {
		return true, nil
	}
	at := s.now()
	if asOf != nil {
		at = *asOf
	}
	if window.EndsAt != nil && !window.EndsAt.After(at) {
		return true, nil
	}
	command, err := destructive.CommandFromContext(ctx, destructive.RevokeAccess, accessID.String(), "access-revoke:"+accessID.String(), struct {
		Reason models.AccessRevokeReason
		AsOf   *time.Time
	}{reason, asOf})
	if err != nil {
		return true, err
	}
	err = destructive.RunLocal(ctx, s.db, s.destructiveAdmissions, command, func(ctx context.Context, _ pgx.Tx, txDB *db.DB) (bool, error) {
		if _, err := txDB.Gen(ctx).LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: command.MerchantID, ID: window.CustomerID}); err != nil {
			return false, err
		}
		local := entitlements.NewEntitlementService(txDB, s.clock)
		current, err := local.GetAccessByID(ctx, accessID)
		if err != nil {
			return false, err
		}
		if current.RevokedAt != nil || current.DeletedAt != nil || current.EndsAt != nil && !current.EndsAt.After(at) {
			return false, nil
		}
		if asOf != nil {
			err = local.RevokeAccess(ctx, entitlements.RevokeAccessParams{AccessID: &accessID, Reason: reason, AsOf: asOf})
		} else {
			err = local.RevokeGrantedAccess(ctx, current, reason)
		}
		if err != nil {
			return true, err
		}
		return true, convergejob.Queue(ctx, txDB, billing.MerchantID(command.MerchantID), window.CustomerID)
	})
	return true, err
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

// Filter selects windows: of these customers and products (nil: any), live
// now when LiveOnly.
type Filter struct {
	Customers []uuid.UUID
	Products  []uuid.UUID
	LiveOnly  bool
}

// ListPage returns one page of windows, newest first: those live now when
// f.LiveOnly, else every window that was not removed. more: another page
// follows.
func (s *Service) ListPage(ctx context.Context, f Filter, after *uuid.UUID, limit int) ([]gen.ListProductAccessPageRow, bool, error) {
	if limit < 1 || limit > 500 {
		return nil, false, errors.New("limit must be between 1 and 500")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	rows, err := s.db.Gen(ctx).ListProductAccessPage(ctx, gen.ListProductAccessPageParams{
		MerchantID: mid.UUID(), CustomerIds: f.Customers, ProductIds: f.Products, LiveOnly: f.LiveOnly, AtTime: s.now().UTC(), AfterID: after, FetchLimit: int32(limit + 1),
	})
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// ListByIDs reads named windows, newest first.
func (s *Service) ListByIDs(ctx context.Context, ids []uuid.UUID) ([]gen.ListProductAccessPageRow, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListProductAccessViews(ctx, gen.ListProductAccessViewsParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	out := make([]gen.ListProductAccessPageRow, len(rows))
	for i, row := range rows {
		out[i] = gen.ListProductAccessPageRow(row)
	}
	slices.SortFunc(out, func(a, b gen.ListProductAccessPageRow) int { return bytes.Compare(b.ID[:], a.ID[:]) })
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

// SetDestructiveAdmissions wires the shared staff-command admission boundary.
func (s *Service) SetDestructiveAdmissions(a *destructive.Admissions) { s.destructiveAdmissions = a }
