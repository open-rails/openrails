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
	"github.com/open-rails/openrails/pkg/billingauth"
	tenantpkg "github.com/open-rails/openrails/pkg/tenant"
)

// Issue #339: the self-service surface mounts when EITHER the control plane
// (the default delegated-token verifier) OR a host-supplied
// DelegatedAuthenticator is configured. These tests pin the gate at the
// server-registration level without a live control plane.

func TestRegisterSelfServiceRoutes_NotMountedWithoutAnyDelegatedIdentitySource(t *testing.T) {
	gin.SetMode(gin.TestMode)

	srv := &Server{cfg: &config.Config{}} // no control plane, no host authenticator
	e := gin.New()
	srv.registerSelfServiceRoutes(e)

	req := httptest.NewRequest(http.MethodGet, "/v1/self/status", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotFound, w.Code)
}

type mountTestDelegatedAuthenticator struct{}

func (mountTestDelegatedAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		TenantID:    tenantpkg.DefaultID.String(),
		SubjectID:   "11111111-1111-1111-1111-111111111111",
		Permissions: []string{controlplane.PermSelfBillingRead},
	}, nil
}

func TestRegisterSelfServiceRoutes_MountedWithHostDelegatedAuthenticatorOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// The previously impossible case: control plane nil, host authenticator set.
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
