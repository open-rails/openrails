package handlers

import (
	"errors"
	"net/http"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/pspimport"
)

func pspImporter(r *httprequest.Request) (*pspimport.Importer, bool) {
	im := r.State.PSPImporter()
	if im == nil || im.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "PSP imports unavailable")
		return nil, false
	}
	return im, true
}

// CreatePSPImport handles POST /v1/merchant/psp-imports: stage a PSP and
// queue its read-only pull.
func CreatePSPImport(r *httprequest.Request) {
	var params billing.CreatePSPImportParams
	if !r.BindJSON(&params) {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.Create(r.Request.Context(), params)
	if err != nil {
		writePSPImportRefusal(r, err, "PSP import failed")
		return
	}
	r.JSON(http.StatusAccepted, out)
}

// writePSPImportRefusal answers a step that could not be queued as
// unavailable: nothing of it was kept.
func writePSPImportRefusal(r *httprequest.Request, err error, internalMessage string) {
	if errors.Is(err, db.ErrNoRiverProducer) {
		r.ErrorCode(billing.CodeServiceUnavailable, "PSP import unavailable: no job runner")
		return
	}
	writeRefusal(r, err, internalMessage)
}

func ListPSPImports(r *httprequest.Request) {
	var params billing.PSPImportListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	if _, err := billing.ParsePSPRef(params.PSP); err != nil {
		r.APIError(api.Coded(billing.CodeInvalidQuery, "psp names a PSP by id or key").WithParam("psp"))
		return
	}
	out, err := im.List(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "PSP import list failed")
		return
	}
	r.SuccessJSON(out)
}

func GetPSPImport(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.Get(r.Request.Context(), id)
	if err != nil {
		writeRefusal(r, err, "PSP import read failed")
		return
	}
	r.SuccessJSON(out)
}

func UpdatePSPImport(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	var params billing.UpdatePSPImportParams
	if !r.BindJSON(&params) {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.Update(r.Request.Context(), id, params)
	if err != nil {
		writeRefusal(r, err, "PSP import update failed")
		return
	}
	r.SuccessJSON(out)
}

func DeletePSPImport(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	if err := im.Delete(r.Request.Context(), id); err != nil {
		writeRefusal(r, err, "PSP import delete failed")
		return
	}
	r.NoContent()
}

func ListPSPImportRecords(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	var params billing.PSPImportRecordListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	if params.IDs, ok = listIDs(r, params.IDs); !ok {
		return
	}
	out, err := im.ListRecords(r.Request.Context(), id, params)
	if err != nil {
		writeRefusal(r, err, "PSP import record list failed")
		return
	}
	r.SuccessJSON(out)
}

// LinkPSPImport handles POST /v1/merchant/psp-imports/{id}/link: it stores
// manual links and price picks and queues the relink.
func LinkPSPImport(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	var params billing.LinkPSPImportParams
	if !r.BindJSON(&params) {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.Link(r.Request.Context(), id, params)
	if err != nil {
		writePSPImportRefusal(r, err, "PSP import link failed")
		return
	}
	r.JSON(http.StatusAccepted, out)
}

// ApplyPSPImport handles POST /v1/merchant/psp-imports/{id}/apply: a person
// queues the landing of the linked records and the approval, in their name,
// of the import's corrections of the kinds they may approve, as proposal
// approval asks (ProposalChangeRoutes): one of the import's open corrections
// they may not approve refuses the request. A subscription whose plan no
// provider price maps stays blocked until a person maps it, links and
// applies again.
func ApplyPSPImport(can StaffCan) func(r *httprequest.Request) {
	return func(r *httprequest.Request) { applyPSPImport(r, can) }
}

func applyPSPImport(r *httprequest.Request, can StaffCan) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.RequestApply(r.Request.Context(), id, personActor(r), changeAuthorizer(r, can))
	var refused staffCanRefusal
	switch {
	case errors.As(err, &refused):
		refuseStaffCan(r, refused.err)
		return
	case err != nil:
		writePSPImportRefusal(r, err, "PSP import apply failed")
		return
	}
	r.JSON(http.StatusAccepted, out)
}

func SetPSPImportMappings(r *httprequest.Request) {
	var params billing.SetPSPImportMappingsParams
	if !r.BindJSON(&params) {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.SetMappings(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "PSP import mappings failed")
		return
	}
	r.SuccessJSON(out)
}

// ListProviderCatalog handles GET /v1/merchant/provider-catalog: one PSP's
// catalog as OpenRails last observed it, each price with the provider price
// mapping it.
func ListProviderCatalog(r *httprequest.Request) {
	var params billing.ProviderCatalogListParams
	if !bindList(r, &params, &params.PageRequest) {
		return
	}
	objects := r.State.ProviderObjects()
	if objects == nil || objects.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "provider catalog unavailable")
		return
	}
	out, err := objects.List(r.Request.Context(), params)
	if err != nil {
		writeRefusal(r, err, "provider catalog list failed")
		return
	}
	r.SuccessJSON(out)
}
