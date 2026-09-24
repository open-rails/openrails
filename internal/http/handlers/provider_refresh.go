package handlers

import (
	"net/http"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RefreshProviders handles POST /v1/merchant/provider-refresh: the bound
// merchant's provider refresh runs now.
func RefreshProviders(r *httprequest.Request) {
	ctx := r.Request.Context()
	mid, ok := merchant.FromContext(ctx)
	if !ok || mid.IsZero() {
		r.ErrorJSON(http.StatusInternalServerError, "merchant unresolved")
		return
	}
	if r.State == nil || r.State.RiverProducer == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "provider refresh unavailable: no job runner")
		return
	}
	id, queued, err := riverjobs.EnqueueMerchantRefresh(ctx, r.State.RiverProducer, mid.UUID(), r.State.ProviderRefreshQueue)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to request provider refresh")
		return
	}
	status := "queued"
	if queued {
		status = "already_running"
	}
	r.JSON(http.StatusAccepted, openrails.ProviderRefresh{Status: status, JobID: id})
}
