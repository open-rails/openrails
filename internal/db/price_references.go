package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// PriceReferences translates private foreign keys for an API projection in a
// single merchant-scoped query. The UUIDs never become public price aliases.
func PriceReferences(ctx context.Context, q *gen.Queries, ids []uuid.UUID) (map[uuid.UUID]billing.PriceID, error) {
	out := make(map[uuid.UUID]billing.PriceID, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListPriceReferences(ctx, gen.ListPriceReferencesParams{MerchantID: mid.UUID(), Ids: ids})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = billing.PriceID(row.PublicID)
	}
	return out, nil
}

// PriceReference resolves a private foreign key for a single response.
func (d *DB) PriceReference(ctx context.Context, id uuid.UUID) (billing.PriceID, error) {
	refs, err := PriceReferences(ctx, d.Gen(ctx), []uuid.UUID{id})
	if err != nil {
		return "", err
	}
	if ref, ok := refs[id]; ok {
		return ref, nil
	}
	return "", pgx.ErrNoRows
}

// ResolvePriceIDs maps canonical IDs to retained private identities. Callers
// then use their existing row loader, which owns catalog visibility rules.
func ResolvePriceIDs(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, ids []string) ([]uuid.UUID, error) {
	rows, err := q.ListPriceStorageIDs(ctx, gen.ListPriceStorageIDsParams{MerchantID: merchantID, Ids: ids})
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out, nil
}
