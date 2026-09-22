package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Catalog action handlers (issue #205/#510). Mounted under
// /merchant/catalog/* with the live merchant:catalog:update permission gate.
//
// Each handler is a thin shim: bind input -> call internal/service facade -> emit
// JSON. The internal/service facade is the canonical surface; embedded callers and
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
	writeRefusal(r, err, "catalog operation failed")
}

// -- Products ----------------------------------------------------------------

func AdminCreateProduct(r *httprequest.Request) {
	var req billingservice.CreateProductRequest
	if !bindCatalogJSON(r, &req) {
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

// AdminEnsureProduct preserves a product's first declaration under its key.
func AdminEnsureProduct(r *httprequest.Request) {
	var req billingservice.CreateProductRequest
	if !bindCatalogJSON(r, &req) {
		return
	}
	key := r.Param("key")
	if req.Key != "" && req.Key != key {
		r.ErrorJSON(http.StatusBadRequest, "product key in path and body must match")
		return
	}
	req.Key = key
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.EnsureProduct(r.Request.Context(), req)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
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
	if raw := r.Query("catalog_id"); raw != "" {
		id, err := openrails.ParseCatalogID(raw)
		if err != nil || id.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid catalog_id")
			return
		}
		catalogID := id.UUID()
		opts.CatalogID = &catalogID
	}
	// archived=false lists live products, archived=true archived ones; absent
	// lists both.
	if v := strings.TrimSpace(r.Query("archived")); v != "" {
		archived := parseBool(v)
		opts.Archived = &archived
	}
	page, err := svc.ListProducts(r.Request.Context(), opts)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, page)
}

func AdminGetProduct(r *httprequest.Request) {
	id, err := openrails.ParseProductID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminGetProductByKey(r *httprequest.Request) {
	key := strings.TrimSpace(r.Param("key"))
	if key == "" {
		r.ErrorJSON(http.StatusBadRequest, "key required")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetProductByKey(r.Request.Context(), key)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminUpdateProduct(r *httprequest.Request) {
	id, err := openrails.ParseProductID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	var req billingservice.UpdateProductRequest
	if !bindCatalogJSON(r, &req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdateProduct(r.Request.Context(), id, req)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminActivateProduct(r *httprequest.Request) {
	id, err := openrails.ParseProductID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ActivateProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminDeactivateProduct(r *httprequest.Request) {
	id, err := openrails.ParseProductID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid product id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.DeactivateProduct(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// -- Prices ------------------------------------------------------------------

func AdminCreatePrice(r *httprequest.Request) {
	var req billingservice.CreatePriceRequest
	if !bindCatalogJSON(r, &req) {
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
	filter := catalog.PriceFilter{
		Currency: moneyutil.NormalizeCurrency(r.Query("currency")),
		Type:     strings.TrimSpace(r.Query("type")),
	}
	if raw := r.Query("catalog_id"); raw != "" {
		id, err := openrails.ParseCatalogID(raw)
		if err != nil || id.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid catalog_id")
			return
		}
		catalogID := id.UUID()
		filter.CatalogID = &catalogID
	}
	if raw := strings.TrimSpace(r.Query("product_id")); raw != "" {
		id, err := openrails.ParseProductID(raw)
		if err != nil || id.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid product_id")
			return
		}
		productID := id.UUID()
		filter.ProductID = &productID
	}
	// archived=false lists live prices, archived=true archived ones; absent
	// lists both.
	if v := strings.TrimSpace(r.Query("archived")); v != "" {
		archived := parseBool(v)
		filter.Archived = &archived
	}
	limit := parseIntDefault(r.Query("limit"), 100)
	offset := parseIntDefault(r.Query("offset"), 0)
	page, err := svc.ListPrices(r.Request.Context(), filter, limit, offset)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, page)
}

func AdminGetPrice(r *httprequest.Request) {
	id, err := openrails.ParsePriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetPrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	if parseBool(r.Query("verify")) {
		if states, vErr := svc.VerifyPriceSync(r.Request.Context(), id.UUID()); vErr == nil && len(states) > 0 {
			out.Providers = states
		}
	}
	r.JSON(http.StatusOK, out)
}

func AdminUpdatePrice(r *httprequest.Request) {
	id, err := openrails.ParsePriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	var req billingservice.UpdatePriceRequest
	if !bindCatalogJSON(r, &req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.UpdatePrice(r.Request.Context(), id, req)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminActivatePrice(r *httprequest.Request) {
	id, err := openrails.ParsePriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.ActivatePrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

func AdminDeactivatePrice(r *httprequest.Request) {
	id, err := openrails.ParsePriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.DeactivatePrice(r.Request.Context(), id)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// AdminGetPriceByKey resolves a price by its #774 key — the CURRENT
// (non-archived) row for that key.
func AdminGetPriceByKey(r *httprequest.Request) {
	key := strings.TrimSpace(r.Param("key"))
	if key == "" {
		r.ErrorJSON(http.StatusBadRequest, "key required")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.GetPriceByKey(r.Request.Context(), key)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// AdminGetPriceKeyHistory returns a price key's full version chain resolved
// from the #774 pointer-movement log (most-recent-first) — the #777 console
// price page's "version chain with dates" surface. Not part of #774's
// original HTTP surface (which only exposed by-key resolution + relabel).
func AdminGetPriceKeyHistory(r *httprequest.Request) {
	key := strings.TrimSpace(r.Param("key"))
	if key == "" {
		r.ErrorJSON(http.StatusBadRequest, "key required")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	items, err := svc.GetPriceKeyHistory(r.Request.Context(), key)
	if err != nil {
		writeCatalogError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[billingservice.PriceKeyHistoryEntry]{
		Items:  items,
		Total:  int64(len(items)),
		Limit:  len(items),
		Offset: 0,
	})
}

type setPriceKeyRequest struct {
	Key string `json:"key"`
}

// AdminSetPriceKey relabels a price's #774 key in place (a plain rename; see
// Service.SetPriceKey for the repoint semantics if the target key is already
// held by another live row).
func AdminSetPriceKey(r *httprequest.Request) {
	id, err := openrails.ParsePriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	var req setPriceKeyRequest
	if !bindCatalogJSON(r, &req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.SetPriceKey(r.Request.Context(), id, req.Key)
	if err != nil {
		writeCatalogError(r, err)
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
