package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/open-rails/openrails/billing"
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
	out, err := im.List(r.Request.Context(), billing.PSPImportListParams{PageRequest: page, IDs: ids})
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
		writeRefusal(r, err, "PSP import link failed")
		return
	}
	r.SuccessJSON(out)
}

// ApplyPSPImport handles POST /v1/merchant/psp-imports/{id}/apply: each plan
// the linked subscriptions name becomes an archived provider price of the
// product the import names for it, then the landing is queued.
func ApplyPSPImport(r *httprequest.Request) {
	id, ok := pathID(r, billing.ParsePSPImportID)
	if !ok {
		return
	}
	im, ok := pspImporter(r)
	if !ok {
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	ctx := r.Request.Context()
	scope, plans, err := im.Planned(ctx, id)
	if err != nil {
		writeRefusal(r, err, "PSP import apply failed")
		return
	}
	keys := make([]string, 0, len(plans))
	for plan := range plans {
		keys = append(keys, plan)
	}
	sort.Strings(keys)
	missing, err := im.MissingProviderPrices(ctx, id, keys)
	if err != nil {
		writeRefusal(r, err, "PSP import apply failed")
		return
	}
	for _, plan := range missing {
		product := plans[plan]
		if product == "" {
			continue // its subscriptions stay blocked, naming the plan
		}
		create := billing.CreateProviderPriceParams{ProductKey: product, Key: scope.Key + "-" + plan, PSP: scope.Key, Archived: true}
		switch scope.Rail {
		case "nmi":
			create.PlanID = plan
		case "stripe":
			create.PriceID = plan
		}
		if _, err := svc.CreateProviderPrice(ctx, create); err != nil {
			writeRefusal(r, fmt.Errorf("plan %q: %w", plan, err), "PSP import apply failed")
			return
		}
	}
	out, err := im.RequestApply(ctx, id)
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
