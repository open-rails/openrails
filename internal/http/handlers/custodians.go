package handlers

import (
	"net/http"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

func ListCustodians(r *httprequest.Request) {
	svc, id, ok := pspContext(r)
	if !ok {
		return
	}
	items, err := svc.CustodianConfigurations(r.Request.Context(), id)
	if err != nil {
		writeRefusal(r, err, "Custodian configuration unavailable")
		return
	}
	r.JSON(http.StatusOK, billing.ListPage[billing.Custodian]{Items: items})
}
