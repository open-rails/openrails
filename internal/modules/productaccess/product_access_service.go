// Package productaccess implements durable, application-facing product
// ownership/access grants (issue #250). It is DISTINCT from feature entitlements
// (openrails.entitlements): a product access grant answers "does this user own
// product X?" and powers purchased-library views, while entitlements model
// feature windows ("premium", "api_access"). A product may carry EntitlementsSpec
// and/or CreditsSpec AND produce a grant.
package productaccess

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/identity"
)

// Service owns the product-access-grant lifecycle.
type Service struct {
	db    *db.DB
	repo  *repo.ProductAccessGrantRepo
	clock clockwork.Clock
}

func NewService(database *db.DB, clocks ...clockwork.Clock) *Service {
	return &Service{
		db:    database,
		repo:  repo.NewProductAccessGrantRepo(database),
		clock: timeutil.FirstClock(clocks...),
	}
}

// SetClock overrides the service clock (tests).
func (s *Service) SetClock(c clockwork.Clock) { s.clock = timeutil.FirstClock(c) }

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// withTx runs fn inside a tenant-scoped transaction so the migration-063 RLS
// policy constrains every query to the request's tenant (TenantTx pins the
// app.tenant_id GUC as the first statement). repo calls inside fn use the
// tx-scoped repo so they ride that GUC.
func (s *Service) withTx(ctx context.Context, fn func(ctx context.Context, r *repo.ProductAccessGrantRepo) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("product access service not initialized")
	}
	return s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, repo.NewProductAccessGrantRepo(s.db.NewWithPgxTx(tx)))
	})
}

// GrantParams describes a product access grant request.
type GrantParams struct {
	UserID     string
	ProductID  uuid.UUID
	SourceType models.ProductAccessSourceType
	// SourceID is the idempotency key component (payment id, admin grant id,
	// subscription id as text). Re-granting the same SourceID is a no-op.
	SourceID  string
	PaymentID *uuid.UUID
	// EndsAt nil => indefinite / durable ownership.
	EndsAt *time.Time
}

// GrantProductAccess creates a durable grant. It is IDEMPOTENT: re-granting the
// same (user, product, source) returns the existing grant unchanged. Returns the
// grant and whether it was newly created.
func (s *Service) GrantProductAccess(ctx context.Context, params GrantParams) (*models.ProductAccessGrant, bool, error) {
	if params.UserID == "" {
		return nil, false, errors.New("user_id is required")
	}
	if (params.ProductID == uuid.UUID{}) {
		return nil, false, errors.New("product_id is required")
	}
	if params.SourceType == "" {
		return nil, false, errors.New("source_type is required")
	}

	now := s.now().UTC()
	var out *models.ProductAccessGrant
	created := false

	err := s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		existing, err := r.GetBySource(ctx, params.UserID, params.ProductID, params.SourceID)
		if err != nil {
			return fmt.Errorf("check existing grant: %w", err)
		}
		if existing != nil {
			out = existing
			return nil
		}

		var endsAt *time.Time
		if params.EndsAt != nil {
			e := params.EndsAt.UTC()
			endsAt = &e
		}
		grant := &models.ProductAccessGrant{
			MerchantSubjectID: identity.MerchantSubjectIDFromString(params.UserID).UUID(),
			ProductID:       params.ProductID,
			SourceType:      params.SourceType,
			SourceID:        params.SourceID,
			PaymentID:       params.PaymentID,
			Status:          models.ProductAccessStatusActive,
			StartsAt:        now,
			EndsAt:          endsAt,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if err := r.Insert(ctx, grant); err != nil {
			return fmt.Errorf("insert grant: %w", err)
		}
		out = grant
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// GetGrant returns a single grant by id (tenant-scoped), or nil if not found.
func (s *Service) GetGrant(ctx context.Context, grantID uuid.UUID) (*models.ProductAccessGrant, error) {
	var grant *models.ProductAccessGrant
	err := s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		g, e := r.GetByID(ctx, grantID)
		if e != nil {
			return e
		}
		grant = g
		return nil
	})
	return grant, err
}

// RevokeProductAccess revokes a single grant by id. Not-found / already-revoked
// is reported via found=false (not an error) so callers can stay idempotent.
func (s *Service) RevokeProductAccess(ctx context.Context, grantID uuid.UUID, reason models.ProductAccessRevokeReason) (found bool, err error) {
	now := s.now().UTC()
	err = s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		n, e := r.RevokeByID(ctx, grantID, now, reason)
		if e != nil {
			return e
		}
		found = n > 0
		return nil
	})
	return found, err
}

// RevokeProductAccessByPayment revokes all active grants tied to a payment.
// Used by the refund / chargeback reversal path. Returns the number revoked.
func (s *Service) RevokeProductAccessByPayment(ctx context.Context, paymentID uuid.UUID, reason models.ProductAccessRevokeReason) (int64, error) {
	now := s.now().UTC()
	var n int64
	err := s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		count, e := r.RevokeByPayment(ctx, paymentID, now, reason)
		if e != nil {
			return e
		}
		n = count
		return nil
	})
	return n, err
}

// HasProductAccess reports whether the user currently has active access to the
// product.
func (s *Service) HasProductAccess(ctx context.Context, userID string, productID uuid.UUID) (bool, error) {
	if userID == "" || (productID == uuid.UUID{}) {
		return false, nil
	}
	at := s.now().UTC()
	var ok bool
	err := s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		has, e := r.HasActiveAccess(ctx, userID, productID, at)
		if e != nil {
			return e
		}
		ok = has
		return nil
	})
	return ok, err
}

// ListAccessibleProducts returns the user's currently-active product access
// grants, most recent first.
func (s *Service) ListAccessibleProducts(ctx context.Context, userID string) ([]models.ProductAccessGrant, error) {
	at := s.now().UTC()
	var grants []models.ProductAccessGrant
	err := s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		out, e := r.ListActiveByUser(ctx, userID, at)
		if e != nil {
			return e
		}
		grants = out
		return nil
	})
	return grants, err
}

// ListAllGrantsByUser returns ALL grants (active + revoked) for a user, most
// recent first. Used by admin views.
func (s *Service) ListAllGrantsByUser(ctx context.Context, userID string) ([]models.ProductAccessGrant, error) {
	var grants []models.ProductAccessGrant
	err := s.withTx(ctx, func(ctx context.Context, r *repo.ProductAccessGrantRepo) error {
		out, e := r.ListByUser(ctx, userID)
		if e != nil {
			return e
		}
		grants = out
		return nil
	})
	return grants, err
}
