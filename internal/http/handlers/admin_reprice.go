package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// Reprices and reprice batches: a reprice moves one subscription to another
// price at its first renewal on or after effective_at; a batch is one bulk
// move (every subscriber on a price key's prior versions, or a plan
// migration).

func writeRepriceError(r *httprequest.Request, err error) {
	writeRefusal(r, err, "reprice operation failed")
}

func repriceServiceReady(r *httprequest.Request) bool {
	if r.State.RepriceService == nil {
		r.ErrorCode(billing.CodeInternalError, "reprice service unavailable")
		return false
	}
	return true
}

func repriceIDParam(r *httprequest.Request) (billing.RepriceID, bool) {
	id, err := billing.ParseRepriceID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid reprice ID").WithParam("id"))
		return id, false
	}
	return id, true
}

func repriceBatchIDParam(r *httprequest.Request) (billing.RepriceBatchID, bool) {
	id, err := billing.ParseRepriceBatchID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid reprice batch ID").WithParam("id"))
		return id, false
	}
	return id, true
}

// CreateRepriceBatch schedules every active subscription on a prior version
// of price_key to move to the key's current price.
func CreateRepriceBatch(r *httprequest.Request) {
	var req billing.CreateRepriceBatchParams
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.PriceKey) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "price_key required").WithParam("price_key"))
		return
	}
	if req.EffectiveAt.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "effective_at required").WithParam("effective_at"))
		return
	}
	if !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.CreateBatch(r.Request.Context(), req)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

// PreviewRepriceBatch counts the subscribers a batch for price_key would
// move, without writing anything.
func PreviewRepriceBatch(r *httprequest.Request) {
	var req billing.PreviewRepriceBatchParams
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.PriceKey) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "price_key required").WithParam("price_key"))
		return
	}
	if !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.PreviewBatch(r.Request.Context(), req.PriceKey)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// RepriceBatchQuery filters GET /reprice-batches.
type RepriceBatchQuery struct {
	PriceKey string `form:"price_key"`
}

// ListRepriceBatches is one page of the merchant's batches, newest first.
func ListRepriceBatches(r *httprequest.Request) {
	var query RepriceBatchQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok || !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.ListBatches(r.Request.Context(), billing.RepriceBatchListParams{PageRequest: page, PriceKey: query.PriceKey})
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// GetRepriceBatch reads one batch with its reprices counted by status.
func GetRepriceBatch(r *httprequest.Request) {
	id, ok := repriceBatchIDParam(r)
	if !ok || !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.GetBatch(r.Request.Context(), id)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// CancelRepriceBatch cancels the batch's still-scheduled reprices.
func CancelRepriceBatch(r *httprequest.Request) {
	id, ok := repriceBatchIDParam(r)
	if !ok || !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.CancelBatch(r.Request.Context(), id)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// RepriceQuery filters GET /reprices.
type RepriceQuery struct {
	SubscriptionID billing.SubscriptionID `form:"subscription_id"`
	RepriceBatchID billing.RepriceBatchID `form:"reprice_batch_id"`
	Status         string                 `form:"status"`
}

// ListReprices is one page of the merchant's reprices, newest first.
func ListReprices(r *httprequest.Request) {
	var query RepriceQuery
	if !r.BindQuery(&query) {
		return
	}
	status := billing.RepriceStatus(query.Status)
	switch status {
	case "", billing.RepriceScheduled, billing.RepriceApplied, billing.RepriceCanceled, billing.RepriceBlocked:
	default:
		r.APIError(api.Coded(billing.CodeInvalidQuery, "status must be scheduled, applied, canceled or blocked").WithParam("status"))
		return
	}
	page, ok := r.Page()
	if !ok || !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.ListReprices(r.Request.Context(), billing.RepriceListParams{
		PageRequest: page, SubscriptionID: query.SubscriptionID, RepriceBatchID: query.RepriceBatchID, Status: status,
	})
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// GetReprice reads one reprice.
func GetReprice(r *httprequest.Request) {
	id, ok := repriceIDParam(r)
	if !ok || !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.GetReprice(r.Request.Context(), id)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSON(out)
}

// CancelReprice cancels a scheduled reprice and answers it.
func CancelReprice(r *httprequest.Request) {
	id, ok := repriceIDParam(r)
	if !ok || !repriceServiceReady(r) {
		return
	}
	out, err := r.State.RepriceService.CancelReprice(r.Request.Context(), id)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSON(out)
}
