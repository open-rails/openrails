package ginmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/tenant"
)

// fakeServiceTokenResolver is a programmable ServiceTokenResolver for the service token middleware tests
// (issue #222).
type fakeServiceTokenResolver struct {
	looksLikeOAT bool
	resolved     *controlplane.ResolvedServiceToken
	err          error
}

func (f fakeServiceTokenResolver) LooksLikeServiceToken(string) bool { return f.looksLikeOAT }

func (f fakeServiceTokenResolver) ResolveServiceToken(context.Context, string) (*controlplane.ResolvedServiceToken, error) {
	return f.resolved, f.err
}

func newOATTestRouter(resolver ServiceTokenResolver, perm string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", ServiceTokenRequired(resolver), RequireServiceTokenPermission(perm), func(c *gin.Context) {
		resolved, _ := ServiceTokenFromGin(c)
		tid, _ := tenant.FromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"org": resolved.AuthKitTenantSlug, "tenant": tid.String()})
	})
	return r
}

func doOATRequest(r *gin.Engine, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/svc", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer openrails_st_keyid_secret")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireServiceTokenTenantSubjectScope(t *testing.T) {
	payer := uuid.New()
	resolver := fakeServiceTokenResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "operator",
			TenantID:          tenant.DefaultID,
			Permissions:       []string{controlplane.PermCreditsSpend},
			Resources: []authcore.ServiceTokenResource{
				controlplane.TenantResource(tenant.DefaultID),
				controlplane.TenantSubjectResource(payer),
			},
		},
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", ServiceTokenRequired(resolver), func(c *gin.Context) {
		if !RequireServiceTokenTenantSubjectScope(c, payer) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)

	r = gin.New()
	r.GET("/svc", ServiceTokenRequired(resolver), func(c *gin.Context) {
		if !RequireServiceTokenTenantSubjectScope(c, uuid.New()) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w = doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_tenant_subject_scope_denied")
}

func TestServiceTokenRequired_SucceedsForCorrectTenantAndPermission(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "operator",
			TenantID:          tenant.DefaultID,
			TenantSlug:        tenant.DefaultSlug,
			Permissions:       []string{controlplane.PermCreditsWrite},
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), tenant.DefaultID.String())
}

func TestServiceTokenRequired_AdminPermissionSatisfiesAnyGate(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "operator",
			TenantID:          tenant.DefaultID,
			Permissions:       []string{controlplane.PermAdmin},
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestServiceTokenRequired_DeniesMissingPermission(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "operator",
			TenantID:          tenant.DefaultID,
			Permissions:       []string{controlplane.PermCreditsRead}, // read only
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_permission_required")
}

func TestServiceTokenRequired_DeniesUnknownPermissionSet(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeOAT: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "operator",
			TenantID:          tenant.DefaultID,
			Permissions:       []string{"openrails:something:unknown"},
		},
	}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestServiceTokenRequired_DeniesExpiredOAT(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeOAT: true, err: authcore.ErrAccessTokenExpired}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_token_expired")
}

func TestServiceTokenRequired_DeniesRevokedOAT(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeOAT: true, err: authcore.ErrAccessTokenRevoked}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_token_revoked")
}

func TestServiceTokenRequired_DeniesUnknownOAT(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeOAT: true, err: authcore.ErrInvalidAccessToken}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_token_invalid")
}

func TestServiceTokenRequired_DeniesCrossTenantOAT(t *testing.T) {
	// Owning org maps to no active tenant for this deployment.
	resolver := fakeServiceTokenResolver{looksLikeOAT: true, err: controlplane.ErrServiceTokenTenantUnresolved}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_tenant_unresolved")
}

func TestServiceTokenRequired_DeniesMissingBearer(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeOAT: true}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServiceTokenRequired_DeniesNonOATCredential(t *testing.T) {
	// A JWT or delegated token is not an service token: these service routes accept only service tokens.
	resolver := fakeServiceTokenResolver{looksLikeOAT: false}
	r := newOATTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServiceTokenRequired_NilResolverFailsClosed(t *testing.T) {
	r := newOATTestRouter(nil, controlplane.PermCreditsWrite)
	w := doOATRequest(r, true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
