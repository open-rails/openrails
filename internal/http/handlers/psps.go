package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// PSPListQuery filters GET /v1/merchant/psps.
type PSPListQuery struct {
	Rail     billing.Rail `form:"rail"`
	Archived *bool        `form:"archived"`
}

// ListPSPs handles GET /v1/merchant/psps.
func ListPSPs(r *httprequest.Request) {
	svc, id, ok := pspContext(r)
	if !ok {
		return
	}
	var query PSPListQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	out, err := svc.ListPSPs(r.Request.Context(), id, billing.PSPListParams{Rail: query.Rail, Archived: query.Archived, PageRequest: page})
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// GetPSP handles GET /v1/merchant/psps/{psp_id}.
func GetPSP(r *httprequest.Request) {
	svc, id, pspID, ok := pspPathContext(r)
	if !ok {
		return
	}
	out, err := svc.GetPSP(r.Request.Context(), id, pspID)
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
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusCreated, out)
}

// UpdatePSP handles PATCH /v1/merchant/psps/{psp_id}.
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
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ArchivePSP handles POST /v1/merchant/psps/{psp_id}/archive. The body is
// optional; no provider call is made, so a dark account archives too.
func ArchivePSP(r *httprequest.Request) {
	svc, id, pspID, ok := pspPathContext(r)
	if !ok {
		return
	}
	var req billing.ArchivePSPParams
	if !r.BindOptionalJSON(&req) {
		return
	}
	out, err := svc.ArchivePSP(r.Request.Context(), id, pspID, req)
	if err != nil {
		writePSPError(r, err)
		return
	}
	r.JSON(http.StatusOK, out)
}

// ListRails handles GET /v1/merchant/rails.
func ListRails(r *httprequest.Request) {
	r.JSON(http.StatusOK, billing.ListPage[billing.RailDefinition]{Items: merchants.RailDefinitions()})
}

// RefreshPSPs handles POST /v1/merchant/psps/refresh: the merchant's PSP
// pull runs now.
func RefreshPSPs(r *httprequest.Request) {
	ctx := r.Request.Context()
	mid, ok := merchant.FromContext(ctx)
	if !ok || mid.IsZero() {
		r.ErrorCode(billing.CodeInternalError, "merchant unresolved")
		return
	}
	if r.State == nil || r.State.RiverProducer == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "PSP refresh unavailable: no job runner")
		return
	}
	id, queued, err := riverjobs.EnqueueMerchantRefresh(ctx, r.State.RiverProducer, mid.UUID(), r.State.ProviderRefreshQueue)
	if err != nil {
		r.InternalError("failed to request PSP refresh", err)
		return
	}
	status := "queued"
	if queued {
		status = "already_running"
	}
	r.JSON(http.StatusAccepted, billing.PSPRefresh{Status: status, JobID: id})
}

func pspPathContext(r *httprequest.Request) (*merchants.Service, billing.MerchantID, billing.PSPID, bool) {
	svc, id, ok := pspContext(r)
	if !ok {
		return nil, billing.MerchantID{}, billing.PSPID{}, false
	}
	pspID, err := billing.ParsePSPID(r.Param("psp_id"))
	if err != nil || pspID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "psp_id must be a psp_ id").WithParam("psp_id"))
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
	switch {
	case errors.As(err, &lastActive):
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, "psp_last_active", err.Error()).
			WithMetadata(map[string]any{"psp_id": lastActive.PSP.String()}))
	case errors.Is(err, merchants.ErrSecretBackendUnavailable):
		r.ErrorCode(billing.CodeServiceUnavailable, "secret backend unavailable")
	default:
		writeRefusal(r, err, "PSP operation failed")
	}
}
