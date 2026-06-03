package ginmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
)

// fakeOATResolver is a programmable OATResolver for the OAT middleware tests
// (issue #222).
type fakeOATResolver struct {
	looksLikeOAT bool
	resolved     *controlplane.ResolvedOAT
	err          error
}

func (f fakeOATResolver) LooksLikeOAT(string) bool { return f.looksLikeOAT }

func (f fakeOATResolver) ResolveOAT(context.Context, string) (*controlplane.ResolvedOAT, error) {
	return f.resolved, f.err
}

func newOATTestRouter(resolver OATResolver, perm string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", OATRequired(resolver), RequireOATPermission(perm), func(c *gin.Context) {
		resolved, _ := OATFromGin(c)
		tid, _ := tenant.FromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"org": resolved.OrgSlug, "tenant": tid.String()})
	})
	return r
}

func doOATRequest(r *gin.Engine, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/svc", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer openrails_oat_keyid_secret")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestOATRequired_SucceedsForCorrectTenantAndPermission(t *testing.T) {
	resolver := fakeOATResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedOAT{
			OrgSlug:     "operator",
			TenantID:    tenant.DefaultID,
			TenantSlug:  tenant.DefaultSlug,
			Permissions: []string{controlplane.PermCreditsWrite},
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), tenant.DefaultID.String())
}

func TestOATRequired_AdminPermissionSatisfiesAnyGate(t *testing.T) {
	resolver := fakeOATResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedOAT{
			OrgSlug:     "operator",
			TenantID:    tenant.DefaultID,
			Permissions: []string{controlplane.PermAdmin},
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestOATRequired_DeniesMissingPermission(t *testing.T) {
	resolver := fakeOATResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedOAT{
			OrgSlug:     "operator",
			TenantID:    tenant.DefaultID,
			Permissions: []string{controlplane.PermCreditsRead}, // read only
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "oat_permission_required")
}

func TestOATRequired_DeniesUnknownPermissionSet(t *testing.T) {
	resolver := fakeOATResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedOAT{
			OrgSlug:     "operator",
			TenantID:    tenant.DefaultID,
			Permissions: []string{"openrails:something:unknown"},
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestOATRequired_DeniesExpiredOAT(t *testing.T) {
	resolver := fakeOATResolver{looksLikeOAT: true, err: authcore.ErrAccessTokenExpired}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "oat_expired")
}

func TestOATRequired_DeniesRevokedOAT(t *testing.T) {
	resolver := fakeOATResolver{looksLikeOAT: true, err: authcore.ErrAccessTokenRevoked}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "oat_revoked")
}

func TestOATRequired_DeniesUnknownOAT(t *testing.T) {
	resolver := fakeOATResolver{looksLikeOAT: true, err: authcore.ErrInvalidAccessToken}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "oat_invalid")
}

func TestOATRequired_DeniesCrossTenantOAT(t *testing.T) {
	// Owning org maps to no active tenant for this deployment.
	resolver := fakeOATResolver{looksLikeOAT: true, err: controlplane.ErrOATTenantUnresolved}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "oat_tenant_unresolved")
}

func TestOATRequired_DeniesMissingBearer(t *testing.T) {
	resolver := fakeOATResolver{looksLikeOAT: true}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestOATRequired_DeniesNonOATCredential(t *testing.T) {
	// A JWT or delegated token is not an OAT: these service routes accept only OATs.
	resolver := fakeOATResolver{looksLikeOAT: false}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestOATRequired_NilResolverFailsClosed(t *testing.T) {
	r := newOATTestRouter(nil, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
