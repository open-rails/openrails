package handlers

import (
	"errors"
	"math"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/catalogscope"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

func catalogView(row gen.OpenrailsCatalog) openrails.Catalog {
	return openrails.Catalog{ID: openrails.CatalogID(row.ID), MerchantID: openrails.MerchantID(row.MerchantID), OwnerSubject: row.OwnerSubject, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func OwnCatalog(r *request.Request) {
	if r.Request.Method == http.MethodPut && r.Request.Body != nil && r.Request.Body != http.NoBody {
		var body struct{}
		if !bindCatalogJSON(r, &body) {
			return
		}
	}
	scope, ok := catalogscope.FromContext(r.Request.Context())
	if !ok {
		r.ErrorJSON(http.StatusForbidden, "catalog owner identity required")
		return
	}
	row, err := catalog.NewCatalogRepo(r.State.DB).Get(r.Request.Context(), scope.CatalogID)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, catalogView(row))
}

func EnsureCatalogForOwner(r *request.Request) {
	var body struct {
		OwnerSubject string `json:"owner_subject"`
	}
	if !bindCatalogJSON(r, &body) {
		return
	}
	if err := catalogscope.ValidateSubject(body.OwnerSubject); err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid owner_subject")
		return
	}
	row, err := catalog.NewCatalogRepo(r.State.DB).Ensure(r.Request.Context(), &body.OwnerSubject)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, catalogView(row))
}

func GetCatalog(r *request.Request) {
	id, err := openrails.ParseCatalogID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid catalog_id")
		return
	}
	row, err := catalog.NewCatalogRepo(r.State.DB).Get(r.Request.Context(), id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		r.ErrorJSON(http.StatusNotFound, "catalog not found")
		return
	}
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, catalogView(row))
}

func ListCatalogs(r *request.Request) {
	limit, offset := parseIntDefault(r.Query("limit"), 50), parseIntDefault(r.Query("offset"), 0)
	if limit < 1 || limit > 100 || offset < 0 || offset > math.MaxInt32 {
		r.ErrorJSON(http.StatusBadRequest, "invalid catalog pagination")
		return
	}
	rows, err := catalog.NewCatalogRepo(r.State.DB).List(r.Request.Context(), int32(limit), int32(offset))
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	items := make([]openrails.Catalog, 0, len(rows))
	for _, row := range rows {
		items = append(items, catalogView(row))
	}
	r.JSON(http.StatusOK, struct {
		Items []openrails.Catalog `json:"items"`
	}{Items: items})
}
