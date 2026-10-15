package handlers

import (
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/gen"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
)

func GetAccessTenure(r *httprequest.Request) {
	customer, ok := productAccessCustomer(r)
	if !ok {
		return
	}
	accessTenure(r, customer)
}

func SelfAccessTenure(r *httprequest.Request) {
	customer, ok := selfAccountPayer(r)
	if !ok {
		return
	}
	accessTenure(r, billing.CustomerID(customer))
}

func accessTenure(r *httprequest.Request, customer billing.CustomerID) {
	keys := r.Request.URL.Query()["entitlement"]
	if len(keys) == 0 || len(keys) > billing.MaxEntitlementChecks {
		r.APIError(api.Coded(billing.CodeInvalidParam, fmt.Sprintf("entitlement must name 1 to %d keys", billing.MaxEntitlementChecks)).WithParam("entitlement"))
		return
	}
	keys, err := catalog.NormalizeEntitlements(keys)
	if err != nil {
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()).WithParam("entitlement"))
		return
	}
	at, ok := parseAtQuery(r)
	if !ok {
		return
	}
	now := r.Clock.Now()
	if at.IsZero() {
		at = now
	} else if at.After(now) {
		r.APIError(api.Coded(billing.CodeInvalidParam, "at must not be in the future").WithParam("at"))
		return
	}
	ctx := r.Request.Context()
	mid, err := merchant.Require(ctx)
	if err != nil {
		r.InternalError("failed to resolve merchant", err)
		return
	}
	rows, err := r.State.DB.Gen(ctx).GetAccessTenure(ctx, gen.GetAccessTenureParams{MerchantID: mid.UUID(), CustomerID: customer.UUID(), Entitlements: keys, AtTime: at})
	if err != nil {
		r.InternalError("failed to read access tenure", err)
		return
	}
	out := billing.AccessTenure{AsOf: at.UTC(), Seconds: make(map[string]int64, len(keys))}
	for _, row := range rows {
		out.Seconds[row.Entitlement] = row.Seconds
	}
	r.SuccessJSON(out)
}
