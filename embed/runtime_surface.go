package embed

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Handler returns the selected billing route sets as one net/http handler.
func (r *Runtime) Handler(options embedded.MountOptions) (http.Handler, error) {
	return embedded.MountHandler(r.emb, options)
}

// SelfHandler returns only the /v1/me and /v1/customers surface.
func (r *Runtime) SelfHandler(authenticator billingauth.DelegatedAuthenticator) (http.Handler, error) {
	return embedded.SelfHandler(r.emb, authenticator)
}

// Ready reports the first unhealthy dependency: Postgres, configured Redis,
// the merchant secret backend and, for OpenRails-managed River, its consumer.
func (r *Runtime) Ready(ctx context.Context) error {
	return r.emb.Ready(ctx)
}

// CheckJobProgress reports whether the billing River fleet is progressing,
// without requiring a job to run.
func (r *Runtime) CheckJobProgress(ctx context.Context) (embedded.JobProgress, error) {
	return r.emb.CheckJobProgress(ctx)
}

// HasExternalRiverClient reports whether the host owns the River client.
func (r *Runtime) HasExternalRiverClient() bool {
	return r.emb.HasExternalRiverClient()
}

// DeclarePSP records a PSP identity without credentials, before importing
// billing facts attributed to it.
func (r *Runtime) DeclarePSP(ctx context.Context, merchantID merchant.ID, declaration embedded.PSPDeclaration) (uuid.UUID, error) {
	return r.emb.DeclarePSP(ctx, merchantID, declaration)
}
