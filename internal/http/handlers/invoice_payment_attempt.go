package handlers

import (
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

func GetMyInvoicePaymentAttempt(r *httprequest.Request) {
	actor := invoiceActor(r, false)
	if actor == nil {
		return
	}
	readInvoicePaymentAttempt(r, actor)
}

func GetInvoicePaymentAttempt(r *httprequest.Request) { readInvoicePaymentAttempt(r, nil) }

func readInvoicePaymentAttempt(r *httprequest.Request, actor *billingservice.InvoiceActor) {
	id, ok := invoiceID(r)
	if !ok {
		return
	}
	key := invoiceKey(r)
	if key == "" {
		return
	}
	svc := invoiceService(r)
	if svc == nil {
		return
	}
	receipt, err := svc.GetInvoicePaymentAttempt(r.Request.Context(), actor, id, key)
	if err != nil {
		writeInvoiceError(r, err)
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	r.JSON(http.StatusOK, receipt)
}
