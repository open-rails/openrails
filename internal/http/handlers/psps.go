package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

// ListPSPs handles GET /v1/merchant/psps.
func ListPSPs(r *httprequest.Request) {
	svc, id, ok := pspContext(r)
	if !ok {
		return
	}
	var params billing.PSPListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	out, err := svc.ListPSPs(r.Request.Context(), id, params)
	if err == nil {
		err = markArmed(r, svc, id, out.Items)
	}
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// GetPSP handles GET /v1/merchant/psps/{id}.
func GetPSP(r *httprequest.Request) {
	svc, id, pspID, ok := pspPathContext(r)
	if !ok {
		return
	}
	out, err := svc.GetPSP(r.Request.Context(), id, pspID)
	if err == nil {
		out, err = markArmedOne(r, svc, id, out)
	}
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// CreatePSP handles POST /v1/merchant/psps.
func CreatePSP(r *httprequest.Request) {
	svc, id, ok := pspContext(r)
	if !ok {
		return
	}
	var req billing.CreatePSPParams
	if !r.BindJSON(&req) {
		return
	}
	out, err := svc.CreatePSP(r.Request.Context(), id, req)
	if err == nil {
		out, err = markArmedOne(r, svc, id, out)
	}
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

// UpdatePSP handles PATCH /v1/merchant/psps/{id}.
func UpdatePSP(r *httprequest.Request) {
	svc, id, pspID, ok := pspPathContext(r)
	if !ok {
		return
	}
	var req billing.UpdatePSPParams
	if !r.BindJSON(&req) {
		return
	}
	out, err := svc.UpdatePSP(r.Request.Context(), id, pspID, req)
	if err == nil {
		out, err = markArmedOne(r, svc, id, out)
	}
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// markArmed sets each PSP's Armed as the payment page decides it.
func markArmed(r *httprequest.Request, svc *merchants.Service, id billing.MerchantID, psps []billing.PSP) error {
	ids := make([]billing.PSPID, 0, len(psps))
	for _, p := range psps {
		ids = append(ids, p.ID)
	}
	armed, err := svc.ArmedPSPs(r.Request.Context(), id, ids, pspArmed(r.State.RailConfigs))
	if err != nil {
		return err
	}
	for i := range psps {
		psps[i].Armed = armed[psps[i].ID]
	}
	return nil
}

func markArmedOne(r *httprequest.Request, svc *merchants.Service, id billing.MerchantID, psp billing.PSP) (billing.PSP, error) {
	one := []billing.PSP{psp}
	err := markArmed(r, svc, id, one)
	return one[0], err
}

func pspPathContext(r *httprequest.Request) (*merchants.Service, billing.MerchantID, billing.PSPID, bool) {
	svc, id, ok := pspContext(r)
	if !ok {
		return nil, billing.MerchantID{}, billing.PSPID{}, false
	}
	pspID, err := billing.ParsePSPID(r.Param("id"))
	if err != nil || pspID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid PSP id").WithParam("id"))
		return nil, billing.MerchantID{}, billing.PSPID{}, false
	}
	return svc, id, pspID, true
}

func pspContext(r *httprequest.Request) (*merchants.Service, billing.MerchantID, bool) {
	if r.State == nil || r.State.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "PSP configuration unavailable")
		return nil, billing.MerchantID{}, false
	}
	id, ok := merchant.FromContext(r.Request.Context())
	if !ok {
		r.ErrorCode(billing.CodeInternalError, "merchant context missing")
		return nil, billing.MerchantID{}, false
	}
	return r.State.Merchants, id, true
}

func writePSPError(r *httprequest.Request, err error) {
	var lastActive *merchants.LastActivePSPError
	var limited *merchants.ProbeLimitedError
	switch {
	case errors.As(err, &lastActive):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodePSPLastActive, err.Error()).
			WithMetadata(map[string]any{"psp_id": lastActive.PSP.String()}))
	case errors.As(err, &limited):
		seconds := int64((limited.RetryAfter + time.Second - 1) / time.Second)
		r.SetHeader("Retry-After", strconv.FormatInt(seconds, 10))
		r.APIError(api.NewAPIError(http.StatusTooManyRequests, api.ErrorTypeRateLimit, billing.CodePSPProbeLimited, err.Error()).
			WithMetadata(map[string]any{"retry_after_seconds": seconds}))
	case errors.Is(err, merchants.ErrConfigUnavailable):
		r.ErrorCode(billing.CodeServiceUnavailable, "merchant configuration unavailable")
	default:
		writeRefusal(r, err, "PSP operation failed")
	}
}
