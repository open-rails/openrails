package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Issue #339: the self-service surface is always mounted (#469); a
// host-supplied DelegatedAuthenticator overrides the control plane's
// delegated-token verifier. This test pins the override at the
// server-registration level without a live control plane.

type mountTestDelegatedAuthenticator struct{}

func (mountTestDelegatedAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		TenantID:    dbtest.TestTenantID.String(),
		SubjectID:   "11111111-1111-1111-1111-111111111111",
		Permissions: []string{controlplane.PermSelfBillingRead},
	}, nil
}

func TestRegisterSelfServiceRoutes_MountedWithHostDelegatedAuthenticatorOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Host authenticator set: it takes precedence, so no control plane is
	// needed to register the surface in this unit test.
	srv := &Server{cfg: &config.Config{}, delegatedAuthenticator: mountTestDelegatedAuthenticator{}}
	e := gin.New()
	srv.registerSelfServiceRoutes(e)

	// A write route the read-only principal cannot pass: 403 proves the surface
	// is MOUNTED and the host principal was ACCEPTED past authentication
	// (unmounted would 404; rejected principal would 401).
	req := httptest.NewRequest(http.MethodPost, "/v1/self/checkout", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// The tenant-admin surface mounts alongside, gated the same way.
	req = httptest.NewRequest(http.MethodGet, "/v1/tenant-admin/subscriptions", nil)
	w = httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
