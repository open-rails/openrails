package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/api"
)

// #773 reprice HTTP surface: schedule/list/cancel a subscription price move,
// plus the bulk reprice_all_prior_versions(key, effective_date) operation.
// Mounted under /v1/merchant/* (session + service-token authz per the
// existing route patterns — see routes.go registerMerchantSupportRoutes).

func writeRepriceError(r *httprequest.Request, err error) {
	if err == nil {
		return
	}
	var constraintErr *subscriptions.RepriceConstraintError
	if errors.As(err, &constraintErr) {
		r.APIError(&api.APIError{
			HTTPStatus: http.StatusUnprocessableEntity,
			Type:       api.ErrorTypeInvalidRequest,
			Code:       repriceErrorCode(constraintErr.Sentinel),
			Message:    err.Error(),
		})
		return
	}
	switch {
	case errors.Is(err, subscriptions.ErrRepriceAlreadyScheduled):
		r.APIError(&api.APIError{HTTPStatus: http.StatusConflict, Type: api.ErrorTypeInvalidRequest, Code: "reprice_already_scheduled", Message: err.Error()})
	case errors.Is(err, subscriptions.ErrRepriceNotScheduled):
		r.APIError(&api.APIError{HTTPStatus: http.StatusConflict, Type: api.ErrorTypeInvalidRequest, Code: "reprice_not_scheduled", Message: err.Error()})
	case strings.Contains(strings.ToLower(err.Error()), "not found"):
		r.ErrorJSON(http.StatusNotFound, err.Error())
	default:
		r.ErrorJSON(http.StatusInternalServerError, err.Error())
	}
}

func repriceErrorCode(sentinel error) string {
	switch {
	case errors.Is(sentinel, subscriptions.ErrRepriceCrossProduct):
		return "reprice_cross_product"
	case errors.Is(sentinel, subscriptions.ErrRepriceCrossCurrency):
		return "reprice_cross_currency"
	case errors.Is(sentinel, subscriptions.ErrRepriceInactivePrice):
		return "reprice_inactive_price"
	default:
		return "reprice_constraint_violation"
	}
}

type createSubscriptionRepriceRequest struct {
	// ToPrice accepts either a price UUID/opaque id or a #774 price_key.
	ToPrice     string    `json:"to_price"`
	EffectiveAt time.Time `json:"effective_at"`
}

// CreateSubscriptionReprice schedules subscription.PriceID -> to_price,
// effective at the subscription's first renewal on/after effective_at.
func CreateSubscriptionReprice(r *httprequest.Request) {
	subscriptionID, err := api.ParseSubscriptionID(r.Param("id"))
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid subscription id")
		return
	}
	var req createSubscriptionRepriceRequest
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.ToPrice) == "" {
		r.ErrorJSON(http.StatusBadRequest, "to_price required")
		return
	}
	if req.EffectiveAt.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "effective_at required")
		return
	}
	if r.State.RepriceService == nil || r.State.PriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	ctx := r.Request.Context()
	toPrice, err := catalog.ResolveReference(ctx, r.State.PriceService, req.ToPrice)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "to_price not found")
		return
	}
	out, err := r.State.RepriceService.Reprice(ctx, subscriptions.RepriceRequest{
		SubscriptionID: subscriptionID,
		ToPriceID:      toPrice.ID,
		EffectiveAt:    req.EffectiveAt,
	})
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

type repriceAllPriorVersionsRequest struct {
	PriceKey    string    `json:"price_key"`
	EffectiveAt time.Time `json:"effective_at"`
}

// RepriceAllPriorVersions bulk-schedules every active subscription pinned to
// a prior version of price_key to move to its current price.
func RepriceAllPriorVersions(r *httprequest.Request) {
	var req repriceAllPriorVersionsRequest
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.PriceKey) == "" {
		r.ErrorJSON(http.StatusBadRequest, "price_key required")
		return
	}
	if req.EffectiveAt.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "effective_at required")
		return
	}
	if r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	out, err := r.State.RepriceService.RepriceAllPriorVersions(r.Request.Context(), subscriptions.RepriceAllPriorVersionsRequest{
		PriceKey:    req.PriceKey,
		EffectiveAt: req.EffectiveAt,
	})
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

// PreviewRepriceAllPriorVersions is #777's read-only dry-run counterpart to
// RepriceAllPriorVersions: returns the affected-count WITHOUT scheduling
// anything — the console wizard's Step 2 preview, called before the price
// edit that would create the new version. Not part of #773's original
// surface (which only exposed the mutating bulk call).
func PreviewRepriceAllPriorVersions(r *httprequest.Request) {
	key := strings.TrimSpace(r.Query("price_key"))
	if key == "" {
		r.ErrorJSON(http.StatusBadRequest, "price_key required")
		return
	}
	if r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	out, err := r.State.RepriceService.PreviewAllPriorVersions(r.Request.Context(), key)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ListRepriceBatchesByKey lists a price key's bulk reprice operations, most
// recent first — the #777 console price page's "is there a pending
// migration for this key" surface (without already knowing a batch id).
func ListRepriceBatchesByKey(r *httprequest.Request) {
	key := strings.TrimSpace(r.Query("price_key"))
	if key == "" {
		r.ErrorJSON(http.StatusBadRequest, "price_key required")
		return
	}
	if r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	limit := parseIntDefault(r.Query("limit"), 20)
	offset := parseIntDefault(r.Query("offset"), 0)
	items, err := r.State.RepriceService.ListBatchesForKey(r.Request.Context(), key, limit, offset)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[*models.RepriceBatch]{
		Items:  items,
		Total:  int64(len(items)),
		Limit:  limit,
		Offset: offset,
	})
}

// ListSubscriptionReprices lists scheduled/applied/canceled reprices, filtered
// by subscription_id / reprice_batch_id / status — the inspect-before-effect
// surface the #777 console wizard needs.
func ListSubscriptionReprices(r *httprequest.Request) {
	if r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	var filter subscriptions.SubscriptionRepriceFilter
	if raw := strings.TrimSpace(r.Query("subscription_id")); raw != "" {
		id, err := api.ParseSubscriptionID(raw)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid subscription_id")
			return
		}
		filter.SubscriptionID = &id
	}
	if raw := strings.TrimSpace(r.Query("reprice_batch_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid reprice_batch_id")
			return
		}
		filter.RepriceBatchID = &id
	}
	if raw := strings.TrimSpace(r.Query("status")); raw != "" {
		status := models.RepriceStatus(raw)
		filter.Status = &status
	}
	limit := parseIntDefault(r.Query("limit"), 100)
	offset := parseIntDefault(r.Query("offset"), 0)
	items, err := r.State.RepriceService.List(r.Request.Context(), filter, limit, offset)
	if err != nil {
		writeRepriceError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[*models.SubscriptionReprice]{
		Items:  items,
		Total:  int64(len(items)),
		Limit:  limit,
		Offset: offset,
	})
}

// GetSubscriptionReprice returns a single reprice row (scheduled, applied, or
// canceled).
func GetSubscriptionReprice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid reprice id")
		return
	}
	if r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	out, err := r.State.RepriceService.GetByID(r.Request.Context(), id)
	if err != nil {
		r.ErrorJSON(http.StatusNotFound, "reprice not found")
		return
	}
	r.JSON(http.StatusOK, out)
}

// CancelSubscriptionReprice cancels a SCHEDULED reprice before it takes
// effect. Refuses (409) if it already applied or was already canceled —
// cancel-before-effective is enforced at the DB layer, so an already-flipped
// subscription is never touched by a late cancel.
func CancelSubscriptionReprice(r *httprequest.Request) {
	id, err := uuid.Parse(strings.TrimSpace(r.Param("id")))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid reprice id")
		return
	}
	if r.State.RepriceService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "reprice service unavailable")
		return
	}
	if err := r.State.RepriceService.Cancel(r.Request.Context(), id); err != nil {
		writeRepriceError(r, err)
		return
	}
	r.SuccessJSONMessage("reprice canceled")
}
