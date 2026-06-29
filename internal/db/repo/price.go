package repo

import (
	"context"
	"errors"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

type PriceRepo struct {
	db *db.DB
}

func NewPriceRepo(d *db.DB) *PriceRepo { return &PriceRepo{db: d} }

func priceRailsJSONB(p *models.Price) ([]byte, error) {
	return toJSONB(p.Rails)
}

func (r *PriceRepo) Create(ctx context.Context, price *models.Price) error {
	rails, err := priceRailsJSONB(price)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).CreatePrice(ctx, gen.CreatePriceParams{
		ID:                  price.ID,
		MerchantID:          price.MerchantID,
		ProductID:           price.ProductID,
		Status:              string(price.Status),
		Amount:              price.Amount,
		Currency:            price.Currency,
		AccessDurationHours: intPtrTo32(price.AccessDurationHours),
		AutoRenew:           price.AutoRenew,
		TrialUnitAmount:     price.TrialUnitAmount,
		TrialDurationHours:  intPtrTo32(price.TrialDurationHours),
		Rails:               rails,
		CreatedAt:           price.CreatedAt,
		UpdatedAt:           price.UpdatedAt,
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PriceRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error) {
	row, err := r.db.Gen(ctx).GetPriceByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return priceFromGen(row)
}

func pricesFromGen(rows []gen.OpenrailsPrice) ([]*models.Price, error) {
	out := make([]*models.Price, 0, len(rows))
	for _, r := range rows {
		p, err := priceFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r *PriceRepo) GetByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	// All statuses (active + archived/draft). The catalog converge relies on this
	// to reconcile already-archived historical prices instead of re-creating
	// them; GetActiveByProductID is the active-only variant.
	rows, err := r.db.Gen(ctx).ListPricesByProduct(ctx, productID)
	if err != nil {
		return nil, err
	}
	return pricesFromGen(rows)
}

func (r *PriceRepo) GetActiveByProductID(ctx context.Context, productID uuid.UUID) ([]*models.Price, error) {
	rows, err := r.db.Gen(ctx).ListActivePricesByProductOrdered(ctx, productID)
	if err != nil {
		return nil, err
	}
	return pricesFromGen(rows)
}

func (r *PriceRepo) GetAllActive(ctx context.Context) ([]*models.Price, error) {
	rows, err := r.db.Gen(ctx).ListAllActivePricesWithProduct(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, err
		}
		out = append(out, price)
	}
	return out, nil
}

func (r *PriceRepo) GetAll(ctx context.Context) ([]*models.Price, error) {
	rows, err := r.db.Gen(ctx).ListAllPricesWithProduct(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, err
		}
		out = append(out, price)
	}
	return out, nil
}

func priceWithProduct(p gen.OpenrailsPrice, prod gen.OpenrailsProduct) (*models.Price, error) {
	price, err := priceFromGen(p)
	if err != nil {
		return nil, err
	}
	product, err := productFromGen(prod)
	if err != nil {
		return nil, err
	}
	price.Product = product
	return price, nil
}

// PriceFilter contains optional filters for listing prices
type PriceFilter struct {
	Active    *bool                 // Filter by active status (true=status='active', false=status<>'active')
	Status    *models.CatalogStatus // Filter by exact lifecycle status (admin)
	Currency  string                // Filter by currency (e.g., "usd")
	ProductID *uuid.UUID            // Filter by product ID
	Type      string                // Filter by "recurring" or "one_time"
}

// ListPaginated returns prices with pagination and optional filters
func (r *PriceRepo) ListPaginated(ctx context.Context, filter PriceFilter, limit, offset int) ([]*models.Price, int64, error) {
	onlyActive := filter.Active != nil && *filter.Active
	onlyInactive := filter.Active != nil && !*filter.Active
	var status *string
	if filter.Status != nil {
		s := string(*filter.Status)
		status = &s
	}
	var currency *string
	if filter.Currency != "" {
		currency = &filter.Currency
	}
	onlyRecurring := filter.Type == "recurring"
	onlyOneTime := filter.Type == "one_time"

	q := r.db.Gen(ctx)
	total, err := q.CountPricesFiltered(ctx, gen.CountPricesFilteredParams{
		OnlyActive:    onlyActive,
		OnlyInactive:  onlyInactive,
		Status:        status,
		Currency:      currency,
		ProductID:     filter.ProductID,
		OnlyRecurring: onlyRecurring,
		OnlyOneTime:   onlyOneTime,
	})
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	rows, err := q.ListPricesFiltered(ctx, gen.ListPricesFilteredParams{
		OnlyActive:    onlyActive,
		OnlyInactive:  onlyInactive,
		Status:        status,
		Currency:      currency,
		ProductID:     filter.ProductID,
		OnlyRecurring: onlyRecurring,
		OnlyOneTime:   onlyOneTime,
		PageLimit:     limit32,
		PageOffset:    offset32,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*models.Price, 0, len(rows))
	for _, row := range rows {
		price, err := priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, price)
	}
	return out, total, nil
}

func (r *PriceRepo) GetByNMIPlan(ctx context.Context, provider, nmiPlanID string) (*models.Price, error) {
	// Resolve any non-draft price (active or archived). Archived prices must
	// still resolve here so grandfathered subscriptions keep openrails.
	row, err := r.db.Gen(ctx).GetPriceByNMIPlan(ctx, gen.GetPriceByNMIPlanParams{
		Provider:           provider,
		PlanID:             nmiPlanID,
		IncludeNmiFallback: provider != "nmi",
	})
	if err != nil {
		return nil, err
	}
	return priceFromGen(row)
}

func (r *PriceRepo) GetByCCBillPriceID(ctx context.Context, ccbillPriceID string) (*models.Price, error) {
	row, err := r.db.Gen(ctx).GetPriceWithProductByCCBillPriceID(ctx, ccbillPriceID)
	if err != nil {
		return nil, err
	}
	return priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
}

func (r *PriceRepo) GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error) {
	row, err := r.db.Gen(ctx).GetPriceWithProductByStripePriceID(ctx, stripePriceID)
	if err != nil {
		return nil, err
	}
	return priceWithProduct(row.OpenrailsPrice, row.OpenrailsProduct)
}

func (r *PriceRepo) Update(ctx context.Context, price *models.Price) error {
	rails, err := priceRailsJSONB(price)
	if err != nil {
		return err
	}
	rows, err := r.db.Gen(ctx).UpdatePrice(ctx, gen.UpdatePriceParams{
		ID:                  price.ID,
		ProductID:           price.ProductID,
		Status:              string(price.Status),
		Amount:              price.Amount,
		Currency:            price.Currency,
		AccessDurationHours: intPtrTo32(price.AccessDurationHours),
		AutoRenew:           price.AutoRenew,
		TrialUnitAmount:     price.TrialUnitAmount,
		TrialDurationHours:  intPtrTo32(price.TrialDurationHours),
		Rails:               rails,
		UpdatedAt:           updateTimestamp(price.UpdatedAt),
	})
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

func (r *PriceRepo) Delete(ctx context.Context, id uuid.UUID) error {
	rows, err := r.db.Gen(ctx).DeletePrice(ctx, id)
	if err != nil {
		return err
	}
	if rows < 1 {
		return errors.New("no rows affected")
	}
	return nil
}

// updateTimestamp keeps a zero UpdatedAt from clobbering the column with
// 0001-01-01 on full-row updates.
func updateTimestamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
