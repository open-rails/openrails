package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Catalog action handlers (issue #205/#510). Mounted under
// /merchant/catalog/* with the live org:catalog:update permission gate.
//
// Each handler is a thin shim: bind input -> call pkg/service facade -> emit
// JSON. The pkg/service facade is the canonical surface; embedded callers and
// HTTP callers go through the same code path.

func newAdminBillingService(r *httprequest.Request) (*billingservice.Service, bool) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return nil, false
	}
	return svc, true
}

func writeCatalogError(r *httprequest.Request, err error) {
	if err == nil {
		return
	}
	// Map known business errors to stable status codes + machine-readable codes.
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not found"):
		r.ErrorJSON(http.StatusNotFound, err.Error())
	case strings.Contains(msg, "required"),
		strings.Contains(msg, "must be positive"),
		strings.Contains(msg, "invalid"):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
	}
}

// -- Products ----------------------------------------------------------------

func AdminCreateProduct(r *httprequest.Request) {
	var req billingservice.CreateProductRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.CreateProduct(r.Request.Context(), req)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

func AdminListProducts(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	opts := billingservice.ListProductsOptions{
		TierGroup: strings.TrimSpace(r.Query("tier_group")),
		Limit:     parseIntDefault(r.Query("limit"), 100),
		Offset:    parseIntDefault(r.Query("offset"), 0),
	}
	if v := strings.TrimSpace(r.Query("active_only")); v != "" {
		opts.ActiveOnly = parseBool(v)
	}
	items, total, err := svc.ListProducts(r.Request.Context(), opts)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[billingservice.CatalogProduct]{
		Items:  items,
		Total:  total,
		Limit:  opts.Limit,
		Offset: opts.Offset,
	})
}

func AdminGetProduct(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, productLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminGetProductBySlug(r *httprequest.Request) {
	slug := strings.TrimSpace(r.Param("slug"))
	if slug == "" {
		r.ErrorJSON(http.StatusBadRequest, "slug required")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetProductBySlug(r.Request.Context(), slug)
	if err != nil {
		writeCatalogError(r, productLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminUpdateProduct(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	var req billingservice.UpdateProductRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdateProduct(r.Request.Context(), id, req)
	if err != nil {
		writeCatalogError(r, productLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminActivateProduct(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ActivateProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, productLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminDeactivateProduct(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.DeactivateProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, productLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

// AdminReconcileProduct diffs the OpenRails product against its Stripe Product
// (discovered via the product's prices) and re-applies OpenRails values
// (display_name, description, active) to Stripe. ?dry_run=true returns the diff
// without mutating. This is the product-level analog of AdminReconcilePrice.
func AdminReconcileProduct(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	opts := billingservice.ReconcileOptions{
		DryRun: parseBool(r.Query("dry_run")),
	}
	out, err := svc.ReconcileProduct(r.Request.Context(), id, opts)
	if err != nil {
		writeCatalogError(r, productLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

// -- Prices ------------------------------------------------------------------

func AdminCreatePrice(r *httprequest.Request) {
	var req billingservice.CreatePriceRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.CreatePrice(r.Request.Context(), req)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

func AdminListPrices(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	// If product_id query param is set, scope to that product's prices.
	if pidRaw := strings.TrimSpace(r.Query("product_id")); pidRaw != "" {
		pid, err := uuid.Parse(pidRaw)
		if err != nil || pid == uuid.Nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid product_id")
			return
		}
		activeOnly := parseBool(r.Query("active_only"))
		items, err := svc.ListPricesByProduct(r.Request.Context(), pid, activeOnly)
		if err != nil {
			writeCatalogError(r, err)
			return
		}
		r.JSON(http.StatusOK, paginatedResponse[billingservice.CatalogPrice]{
			Items:  items,
			Total:  int64(len(items)),
			Limit:  len(items),
			Offset: 0,
		})
		return
	}

	// Otherwise paginate across all prices with filters.
	filter := repo.PriceFilter{
		Currency: strings.ToLower(strings.TrimSpace(r.Query("currency"))),
		Type:     strings.TrimSpace(r.Query("type")),
	}
	if v := strings.TrimSpace(r.Query("active_only")); v != "" {
		active := parseBool(v)
		filter.Active = &active
	}
	limit := parseIntDefault(r.Query("limit"), 100)
	offset := parseIntDefault(r.Query("offset"), 0)
	items, total, err := svc.ListPrices(r.Request.Context(), filter, limit, offset)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[billingservice.CatalogPrice]{
		Items:  items,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

func AdminGetPrice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetPrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, priceLookupErr(err))
		return
	}
	if parseBool(r.Query("verify")) {
		if states, vErr := svc.VerifyPriceSync(r.Request.Context(), id); vErr == nil && len(states) > 0 {
			out.Providers = states
		}
	}
	r.JSON(http.StatusOK, out)
}

func AdminUpdatePrice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	var req billingservice.UpdatePriceRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdatePrice(r.Request.Context(), id, req)
	if err != nil {
		writeCatalogError(r, priceLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminActivatePrice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ActivatePrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, priceLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

// AdminReconcilePrice diffs the OpenRails price against Stripe and re-applies
// OpenRails values to Stripe. ?dry_run=true returns the diff without mutating.
// ?recreate=true is required when the stored Stripe Price 404s.
func AdminReconcilePrice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	opts := billingservice.ReconcileOptions{
		DryRun:   parseBool(r.Query("dry_run")),
		Recreate: parseBool(r.Query("recreate")),
	}
	out, err := svc.ReconcilePrice(r.Request.Context(), id, opts)
	if err != nil {
		writeCatalogError(r, priceLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminDeactivatePrice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.DeactivatePrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, priceLookupErr(err))
		return
	}
	r.JSON(http.StatusOK, out)
}

// -- Helpers -----------------------------------------------------------------

type paginatedResponse[T any] struct {
	Items  []T   `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}

func parseIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// productLookupErr rewrites generic "sql: no rows" / bun "not found" errors
// from the repo layer into a stable "product_not_found" message so the HTTP
// layer can map to 404.
func productLookupErr(err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "no rows") || strings.Contains(low, "not found") {
		return errors.New("product_not_found")
	}
	return err
}

func priceLookupErr(err error) error {
	if err == nil {
		return nil
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "no rows") || strings.Contains(low, "not found") {
		return errors.New("price_not_found")
	}
	return err
}
