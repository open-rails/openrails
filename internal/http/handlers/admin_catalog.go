package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Catalog action handlers (issue #205/#510). Mounted under
// /merchant/catalog/* with the live merchant:catalog:update permission gate.
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
	// or#782: product_not_found/price_not_found (the stable codes the lookup
	// helpers rewrite "sql: no rows" into) match on the UNDERSCORE form — without
	// it every missing-or-foreign id fell through to a 500, so a cross-merchant
	// read was denied while looking like a server fault.
	case strings.Contains(msg, "not found"), strings.Contains(msg, "not_found"):
		r.ErrorJSON(http.StatusNotFound, err.Error())
	case strings.Contains(msg, "duplicate key"),
		strings.Contains(msg, "already exists"):
		// A unique-constraint collision is a client conflict, not a 500 — and the
		// raw Postgres "… (SQLSTATE 23505)" text must never leak to the client (#783).
		r.ErrorJSON(http.StatusConflict, "a resource with these attributes already exists")
	case strings.Contains(msg, "required"),
		strings.Contains(msg, "must be positive"),
		strings.Contains(msg, "must be non-negative"),
		strings.Contains(msg, "invalid"):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		// Never pass raw sql/pgx/SQLSTATE text to the client (#783): log the real
		// error, return a generic message.
		log.WithError(err).Error("catalog operation failed")
		r.ErrorJSON(http.StatusInternalServerError, "internal error")
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
	filter := catalog.PriceFilter{
		Currency: strings.ToLower(strings.TrimSpace(r.Query("currency"))),
		Type:     strings.TrimSpace(r.Query("type")),
	}
	if v := strings.TrimSpace(r.Query("active_only")); v != "" {
		archived := !parseBool(v)
		filter.Archived = &archived
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
		writeCatalogError(r, priceLookupErr(err))
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
		writeCatalogError(r, priceLookupErr(err))
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
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid price id")
		return
	}
	var req setPriceKeyRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	out, err := svc.SetPriceKey(r.Request.Context(), id, req.Key)
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
