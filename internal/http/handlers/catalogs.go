package handlers

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/pagination"
)

// CatalogListQuery filters ListCatalogs.
type CatalogListQuery struct {
	OwnerSubject string `form:"owner_subject"`
}

func catalogView(row gen.BillingCatalog) billing.Catalog {
	return billing.Catalog{ID: billing.CatalogID(row.ID), OwnerSubject: row.OwnerSubject, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

// EnsureCatalog returns the creator's catalog, creating it the first time.
func EnsureCatalog(r *request.Request) {
	if err := catalogpolicy.Check(r.Request.Context(), r.State.Config); err != nil {
		writeCatalogError(r, err)
		return
	}
	var params billing.EnsureCatalogParams
	if !r.BindJSON(&params) {
		return
	}
	if err := catalogscope.ValidateSubject(params.OwnerSubject); err != nil {
		r.APIError(invalidParam("owner_subject", "invalid owner_subject"))
		return
	}
	row, err := catalog.NewCatalogRepo(r.State.DB).Ensure(r.Request.Context(), &params.OwnerSubject)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, catalogView(row))
}

func GetCatalog(r *request.Request) {
	id, err := billing.ParseCatalogID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(invalidParam("id", "invalid catalog id"))
		return
	}
	row, err := catalog.NewCatalogRepo(r.State.DB).Get(r.Request.Context(), id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorCode("catalog_not_found", "")
		return
	}
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, catalogView(row))
}

// ListCatalogs lists the merchant's catalogs, oldest first;
// ?owner_subject= selects one creator's.
func ListCatalogs(r *request.Request) {
	page, ok := r.Page()
	if !ok {
		return
	}
	var query CatalogListQuery
	if !r.BindQuery(&query) {
		return
	}
	rows, err := catalog.NewCatalogRepo(r.State.DB).List(r.Request.Context(), query.OwnerSubject, page)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, pagination.Map(rows, catalogView))
}
