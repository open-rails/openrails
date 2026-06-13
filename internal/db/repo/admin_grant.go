package repo

import (
	"context"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

type AdminGrantRepo struct {
	db *db.DB
}

func NewAdminGrantRepo(db *db.DB) *AdminGrantRepo {
	return &AdminGrantRepo{db: db}
}

func adminGrantFromGen(g gen.OpenrailsAdminGrant) *models.AdminGrant {
	return &models.AdminGrant{
		ID:              g.ID,
		TenantSubjectID: g.TenantSubjectID,
		PriceID:         g.PriceID,
		GrantedBy:       g.GrantedBy,
		Reason:          g.Reason,
		PaymentID:       g.PaymentID,
		DurationDays:    derefIntPtr(g.DurationDays),
		CreatedAt:       g.CreatedAt,
	}
}

// attachAdminGrantRelations loads the Price and Payment relations.
func (r *AdminGrantRepo) attachAdminGrantRelations(ctx context.Context, grants []*models.AdminGrant) error {
	if len(grants) == 0 {
		return nil
	}
	priceIDs := make([]uuid.UUID, 0, len(grants))
	paymentIDs := make([]uuid.UUID, 0, len(grants))
	seenPrice := map[uuid.UUID]bool{}
	seenPayment := map[uuid.UUID]bool{}
	for _, g := range grants {
		if g.PriceID != nil && !seenPrice[*g.PriceID] {
			seenPrice[*g.PriceID] = true
			priceIDs = append(priceIDs, *g.PriceID)
		}
		if g.PaymentID != nil && !seenPayment[*g.PaymentID] {
			seenPayment[*g.PaymentID] = true
			paymentIDs = append(paymentIDs, *g.PaymentID)
		}
	}
	q := r.db.Gen(ctx)
	prices := map[uuid.UUID]*models.Price{}
	if len(priceIDs) > 0 {
		rows, err := q.ListPricesByIDs(ctx, priceIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			p, err := priceFromGen(row)
			if err != nil {
				return err
			}
			prices[p.ID] = p
		}
	}
	payments := map[uuid.UUID]*models.Payment{}
	if len(paymentIDs) > 0 {
		rows, err := q.ListPaymentsByIDs(ctx, paymentIDs)
		if err != nil {
			return err
		}
		for _, row := range rows {
			p, err := paymentFromGen(row)
			if err != nil {
				return err
			}
			payments[p.ID] = p
		}
	}
	for _, g := range grants {
		if g.PriceID != nil {
			g.Price = prices[*g.PriceID]
		}
		if g.PaymentID != nil {
			g.Payment = payments[*g.PaymentID]
		}
	}
	return nil
}

// Create inserts a new admin grant record
func (r *AdminGrantRepo) Create(ctx context.Context, grant *models.AdminGrant) error {
	if err := ensureTenantSubjectRow(ctx, r.db.Qx(ctx), uuid.Nil, grant.TenantSubjectID); err != nil {
		return err
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	id, err := r.db.Gen(ctx).CreateAdminGrant(ctx, gen.CreateAdminGrantParams{
		ID:              grant.ID,
		TenantID:        tid.UUID(),
		TenantSubjectID: grant.TenantSubjectID,
		PriceID:         grant.PriceID,
		GrantedBy:       grant.GrantedBy,
		Reason:          grant.Reason,
		PaymentID:       grant.PaymentID,
		DurationDays:    intPtrTo32(grant.DurationDays),
		CreatedAt:       grant.CreatedAt,
	})
	if err != nil {
		return err
	}
	grant.ID = id
	return nil
}

// GetByID retrieves an admin grant by ID
func (r *AdminGrantRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.AdminGrant, error) {
	row, err := r.db.Gen(ctx).GetAdminGrantByID(ctx, id)
	if err != nil {
		return nil, err
	}
	grant := adminGrantFromGen(row)
	if err := r.attachAdminGrantRelations(ctx, []*models.AdminGrant{grant}); err != nil {
		return nil, err
	}
	return grant, nil
}

// ListByUserID retrieves all admin grants for a user
func (r *AdminGrantRepo) ListByUserID(ctx context.Context, userID string, limit, offset int) ([]models.AdminGrant, int, error) {
	tsid, err := ResolveTenantSubjectID(userID)
	if err != nil {
		return nil, 0, err
	}
	q := r.db.Gen(ctx)
	total, err := q.CountAdminGrantsByTenantSubject(ctx, tsid)
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListAdminGrantsByTenantSubject(ctx, gen.ListAdminGrantsByTenantSubjectParams{
		TenantSubjectID: tsid,
		PageLimit:       limit32,
		PageOffset:      offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	return r.grantsWithRelations(ctx, rows, int(total))
}

// ListByGrantedBy retrieves all admin grants made by a specific admin
func (r *AdminGrantRepo) ListByGrantedBy(ctx context.Context, grantedBy string, limit, offset int) ([]models.AdminGrant, int, error) {
	q := r.db.Gen(ctx)
	total, err := q.CountAdminGrantsByGrantedBy(ctx, grantedBy)
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListAdminGrantsByGrantedBy(ctx, gen.ListAdminGrantsByGrantedByParams{
		GrantedBy:  grantedBy,
		PageLimit:  limit32,
		PageOffset: offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	return r.grantsWithRelations(ctx, rows, int(total))
}

func (r *AdminGrantRepo) grantsWithRelations(ctx context.Context, rows []gen.OpenrailsAdminGrant, total int) ([]models.AdminGrant, int, error) {
	grants := make([]*models.AdminGrant, 0, len(rows))
	for _, row := range rows {
		grants = append(grants, adminGrantFromGen(row))
	}
	if err := r.attachAdminGrantRelations(ctx, grants); err != nil {
		return nil, 0, err
	}
	out := make([]models.AdminGrant, 0, len(grants))
	for _, g := range grants {
		out = append(out, *g)
	}
	return out, total, nil
}
