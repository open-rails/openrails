package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// Price migrations move subscribers to another price at their renewal; a
// subscription's pending move is its scheduled_change, which a change back
// to its current price clears.

func priceMigrationsReady(r *httprequest.Request) bool {
	if r.State.PriceMigrationService == nil {
		r.ErrorCode(billing.CodeInternalError, "price migration service unavailable")
		return false
	}
	return true
}

func priceMigrationIDParam(r *httprequest.Request) (billing.PriceMigrationID, bool) {
	id, err := billing.ParsePriceMigrationID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid price migration ID").WithParam("id"))
		return id, false
	}
	return id, true
}

// CreatePriceMigration moves the subscribers and answers the migration.
func CreatePriceMigration(r *httprequest.Request) {
	var body billing.CreatePriceMigrationParams
	if !r.BindJSON(&body) || !priceMigrationsReady(r) {
		return
	}
	out, err := r.State.PriceMigrationService.Create(r.Request.Context(), body)
	if err != nil {
		writeRefusal(r, err, "price migration failed")
		return
	}
	r.JSON(http.StatusCreated, out)
}

// PreviewPriceMigration answers what creating it would do, writing nothing.
func PreviewPriceMigration(r *httprequest.Request) {
	var body billing.CreatePriceMigrationParams
	if !r.BindJSON(&body) || !priceMigrationsReady(r) {
		return
	}
	out, err := r.State.PriceMigrationService.Preview(r.Request.Context(), body)
	if err != nil {
		writeRefusal(r, err, "price migration preview failed")
		return
	}
	r.SuccessJSON(out)
}

// PriceMigrationQuery filters GET /price-migrations.
type PriceMigrationQuery struct {
	ProductKey string `form:"product_key"`
	PriceKey   string `form:"price_key"`
}

// ListPriceMigrations is one page of the merchant's migrations, newest first.
func ListPriceMigrations(r *httprequest.Request) {
	var query PriceMigrationQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok || !priceMigrationsReady(r) {
		return
	}
	ids, ok := listIDs(r, billing.ParsePriceMigrationID)
	if !ok {
		return
	}
	out, err := r.State.PriceMigrationService.List(r.Request.Context(), billing.PriceMigrationListParams{PageRequest: page, IDs: ids, ProductKey: query.ProductKey, PriceKey: query.PriceKey})
	if err != nil {
		writeRefusal(r, err, "price migration list failed")
		return
	}
	r.SuccessJSON(out)
}

// GetPriceMigration reads one migration with its moves counted.
func GetPriceMigration(r *httprequest.Request) {
	id, ok := priceMigrationIDParam(r)
	if !ok || !priceMigrationsReady(r) {
		return
	}
	out, err := r.State.PriceMigrationService.Get(r.Request.Context(), id)
	if err != nil {
		writeRefusal(r, err, "price migration read failed")
		return
	}
	r.SuccessJSON(out)
}

// CancelPriceMigration cancels its still-scheduled moves.
func CancelPriceMigration(r *httprequest.Request) {
	id, ok := priceMigrationIDParam(r)
	if !ok || !priceMigrationsReady(r) {
		return
	}
	out, err := r.State.PriceMigrationService.Cancel(r.Request.Context(), id)
	if err != nil {
		writeRefusal(r, err, "price migration cancel failed")
		return
	}
	r.SuccessJSON(out)
}
