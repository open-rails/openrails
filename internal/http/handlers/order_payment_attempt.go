package handlers

import (
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

func GetMyOrderPaymentAttempt(r *httprequest.Request) {
	actor := orderActor(r, false)
	if actor == nil {
		return
	}
	readOrderPaymentAttempt(r, actor)
}

func GetOrderPaymentAttempt(r *httprequest.Request) { readOrderPaymentAttempt(r, nil) }

func readOrderPaymentAttempt(r *httprequest.Request, actor *billingservice.OrderActor) {
	id, ok := orderID(r)
	if !ok {
		return
	}
	key := orderKey(r)
	if key == "" {
		return
	}
	svc := orderService(r)
	if svc == nil {
		return
	}
	receipt, err := svc.GetOrderPaymentAttempt(r.Request.Context(), actor, id, key)
	if err != nil {
		writeOrderError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(http.StatusOK, receipt)
}
