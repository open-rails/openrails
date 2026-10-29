package handlers

import (
	"errors"
	"net/http"
	"strings"

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

// PSPImportQuery filters ListPSPImports: psp names one PSP by id or key.
type PSPImportQuery struct {
	PSP string `form:"psp"`
}

func ListPSPImports(r *httprequest.Request) {
	var query PSPImportQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	params := billing.PSPImportListParams{PageRequest: page, IDs: ids}
	if psp := strings.TrimSpace(query.PSP); strings.HasPrefix(psp, billing.PSPIDPrefix) {
		id, err := billing.ParsePSPID(psp)
		if err != nil {
			r.APIError(api.Coded(billing.CodeInvalidQuery, "psp names a PSP by id or key").WithParam("psp"))
			return
		}
		params.PSP.ID = &id
	} else {
		params.PSP.Key = psp
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

// PSPImportRecordQuery filters ListPSPImportRecords.
type PSPImportRecordQuery struct {
	State billing.PSPImportRecordState `form:"state"`
	Kind  billing.PSPImportRecordKind  `form:"kind"`
}

func ListPSPImportRecords(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	var query PSPImportRecordQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	ids, ok := listIDs(r, billing.ParsePSPImportRecordID)
	if !ok {
		return
	}
	out, err := im.ListRecords(r.Request.Context(), id, billing.PSPImportRecordListParams{PageRequest: page, IDs: ids, State: query.State, Kind: query.Kind})
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
// of the import's corrections. A subscription whose plan no provider price
// maps stays blocked until a person maps it, links and applies again.
func ApplyPSPImport(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	out, err := im.RequestApply(r.Request.Context(), id, personActor(r))
	if err != nil {
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

// ProviderCatalogQuery filters ListProviderCatalog.
type ProviderCatalogQuery struct {
	// PSP is the PSP's id or key.
	PSP     string                      `form:"psp"`
	Kind    billing.ProviderCatalogKind `form:"kind"`
	Mapped  *bool                       `form:"mapped"`
	Retired *bool                       `form:"retired"`
	Import  *billing.PSPImportID        `form:"import"`
}

// pspRef reads a PSP named by id (psp_…) or key.
func pspRef(v string) billing.PSPRef {
	v = strings.TrimSpace(v)
	if id, err := billing.ParsePSPID(v); err == nil {
		return billing.PSPRef{ID: &id}
	}
	return billing.PSPRef{Key: v}
}

// ListProviderCatalog handles GET /v1/merchant/provider-catalog: one PSP's
// catalog as OpenRails last observed it, each price with the provider price
// mapping it.
func ListProviderCatalog(r *httprequest.Request) {
	var query ProviderCatalogQuery
	if !r.BindQuery(&query) {
		return
	}
	page, ok := r.Page()
	if !ok {
		return
	}
	objects := r.State.ProviderObjects()
	if objects == nil || objects.Merchants == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "provider catalog unavailable")
		return
	}
	out, err := objects.List(r.Request.Context(), billing.ProviderCatalogListParams{PageRequest: page, PSP: pspRef(query.PSP), Kind: query.Kind, Mapped: query.Mapped, Retired: query.Retired, Import: query.Import})
	if err != nil {
		writeRefusal(r, err, "provider catalog list failed")
		return
	}
	r.SuccessJSON(out)
}
