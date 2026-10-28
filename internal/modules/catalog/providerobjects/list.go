package providerobjects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// position is a catalog list's keyset position.
type position struct {
	Kind string `json:"k"`
	Ref  string `json:"r"`
}

// List reads one PSP's observed catalog for the merchant in ctx.
func (o *Observer) List(ctx context.Context, p billing.ProviderCatalogListParams) (billing.ListPage[billing.ProviderCatalogObject], error) {
	var out billing.ListPage[billing.ProviderCatalogObject]
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	if p.PSP.IsZero() {
		return out, apperr.Invalidf("psp is required").WithParam("psp")
	}
	if o.Merchants == nil {
		return out, errors.New("provider catalog: merchant configuration is not wired")
	}
	var scope merchants.PSPScope
	var found bool
	if p.PSP.ID != nil && !p.PSP.ID.IsZero() {
		scope, found, err = o.Merchants.PSPScopeByID(ctx, mid, p.PSP.ID.UUID())
	} else {
		scope, found, err = o.Merchants.PSPScopeByKey(ctx, mid, strings.TrimSpace(p.PSP.Key))
	}
	if err != nil {
		return out, err
	} else if !found {
		return out, apperr.New(404, "psp_not_found", fmt.Sprintf("PSP %s not found", p.PSP)).WithParam("psp")
	}
	params := gen.ListProviderCatalogParams{MerchantID: mid.UUID(), PspID: scope.ID, Mapped: p.Mapped, Retired: p.Retired}
	switch p.Kind {
	case "":
	case billing.ProviderCatalogProduct, billing.ProviderCatalogPrice:
		params.Kind = new(string(p.Kind))
	default:
		return out, apperr.Invalidf("kind is product or price").WithParam("kind")
	}
	if p.Import != nil && !p.Import.IsZero() {
		row, err := o.DB.Gen(ctx).GetPSPImport(ctx, gen.GetPSPImportParams{MerchantID: mid.UUID(), ID: p.Import.UUID()})
		if errors.Is(err, pgx.ErrNoRows) {
			return out, apperr.New(404, billing.CodeResourceNotFound, "PSP import not found").WithParam("import")
		}
		if err != nil {
			return out, err
		}
		if row.PspID != scope.ID {
			return out, apperr.Invalidf("import %s pulled another PSP", p.Import).WithParam("import")
		}
		params.ImportID = new(row.ID)
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return out, err
	}
	var after position
	if _, err := pagination.Decode(p.Cursor, &after); err != nil {
		return out, err
	}
	params.AfterKind, params.AfterRef, params.RowLimit = after.Kind, after.Ref, pagination.Fetch(limit)
	rows, err := o.DB.Gen(ctx).ListProviderCatalog(ctx, params)
	if err != nil {
		return out, err
	}
	items := make([]billing.ProviderCatalogObject, 0, len(rows))
	for _, r := range rows {
		item := billing.ProviderCatalogObject{Kind: billing.ProviderCatalogKind(r.Kind), Ref: r.Ref, ProductRef: r.ProductRef, Name: r.Name, Active: r.Active,
			DeletedAt: r.DeletedAt, Retired: r.Retired, ObservedAt: r.ObservedAt.UTC(), ObservedVia: r.ObservedVia}
		if item.DeletedAt != nil {
			item.DeletedAt = new(item.DeletedAt.UTC())
		}
		if r.Kind == string(Price) {
			item.Terms = &billing.ProviderCatalogTerms{StatedAmount: r.Amount, Currency: r.Currency, BillingIntervalHours: intOf(r.BillingIntervalHours), BillingIntervalMonths: intOf(r.BillingIntervalMonths)}
			item.Parked = new(int(r.Parked))
			if r.Finding != uuid.Nil {
				item.Finding = new(billing.FindingID(r.Finding))
			}
		}
		if r.Price != "" {
			item.Price = new(billing.PriceID(r.Price))
		}
		if params.ImportID != nil {
			item.Subscriptions = new(int(r.Subscriptions))
		}
		items = append(items, item)
	}
	return pagination.Cut(items, limit, func(v billing.ProviderCatalogObject) any { return position{Kind: string(v.Kind), Ref: v.Ref} }), nil
}

func intOf(v *int32) *int {
	if v == nil {
		return nil
	}
	return new(int(*v))
}
