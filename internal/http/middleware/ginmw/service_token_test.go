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
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeServiceTokenResolver is a programmable ServiceTokenResolver for the service token middleware tests
// (issue #222).
type fakeServiceTokenResolver struct {
	looksLikeServiceToken bool
	resolved              *controlplane.ResolvedServiceToken
	err                   error
	serviceJWTResolved    *controlplane.ResolvedServiceToken
	serviceJWTErr         error
}

func (f fakeServiceTokenResolver) LooksLikeServiceToken(string) bool { return f.looksLikeServiceToken }

func (f fakeServiceTokenResolver) ResolveServiceToken(context.Context, string) (*controlplane.ResolvedServiceToken, error) {
	return f.resolved, f.err
}

func (f fakeServiceTokenResolver) ResolveServiceJWT(context.Context, string) (*controlplane.ResolvedServiceToken, error) {
	if f.serviceJWTResolved != nil || f.serviceJWTErr != nil {
		return f.serviceJWTResolved, f.serviceJWTErr
	}
	return nil, authcore.ErrInvalidServiceJWT
}

func newServiceTokenTestRouter(resolver ServiceTokenResolver, perm string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", ServiceTokenRequired(resolver), RequireServiceTokenPermission(perm), func(c *gin.Context) {
		resolved, _ := ServiceTokenFromGin(c)
		tid, _ := merchant.FromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"authkit_org": resolved.OwnerOrgSlug, "merchant": tid.String()})
	})
	return r
}

func doServiceTokenRequest(r *gin.Engine, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/svc", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer openrails_st_keyid_secret")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireServiceTokenCustomerScope(t *testing.T) {
	payer := uuid.New()
	resolver := fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{controlplane.PermCreditsSpend},
			Resources: []authcore.ServiceTokenResource{
				controlplane.MerchantResource(dbtest.TestMerchantID),
				controlplane.CustomerResource(payer),
			},
		},
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", ServiceTokenRequired(resolver), func(c *gin.Context) {
		if !RequireServiceTokenCustomerScope(c, payer) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)

	r = gin.New()
	r.GET("/svc", ServiceTokenRequired(resolver), func(c *gin.Context) {
		if !RequireServiceTokenCustomerScope(c, uuid.New()) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w = doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_tenant_subject_scope_denied")
}

func TestServiceTokenRequired_SucceedsForCorrectTenantAndPermission(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			MerchantSlug: dbtest.TestMerchantSlug,
			Permissions:  []string{controlplane.PermCreditsWrite},
		},
	}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), dbtest.TestMerchantID.String())
}

func TestServiceTokenRequired_SucceedsForServiceJWT(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeServiceToken: false,
		serviceJWTResolved: &controlplane.ResolvedServiceToken{
			OwnerOrgSlug: "cozy-art",
			MerchantID:   dbtest.TestMerchantID,
			MerchantSlug: dbtest.TestMerchantSlug,
			Permissions:  []string{controlplane.PermCreditsWrite},
		},
	}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), dbtest.TestMerchantID.String())
}

func TestServiceTokenRequired_AdminPermissionSatisfiesAnyGate(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{controlplane.PermAdmin},
		},
	}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestServiceTokenRequired_DeniesMissingPermission(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{controlplane.PermCreditsRead}, // read only
		},
	}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_permission_required")
}

func TestServiceTokenRequired_DeniesUnknownPermissionSet(t *testing.T) {
	resolver := fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{"openrails:something:unknown"},
		},
	}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestServiceTokenRequired_DeniesExpiredServiceToken(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, err: authcore.ErrAccessTokenExpired}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_token_expired")
}

func TestServiceTokenRequired_DeniesRevokedServiceToken(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, err: authcore.ErrAccessTokenRevoked}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_token_revoked")
}

func TestServiceTokenRequired_DeniesUnknownServiceToken(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, err: authcore.ErrInvalidAccessToken}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_token_invalid")
}

func TestServiceTokenRequired_DeniesCrossTenantServiceToken(t *testing.T) {
	// Owning AuthKit org maps to no active OpenRails merchant for this deployment.
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, err: controlplane.ErrServiceTokenMerchantUnresolved}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_merchant_unresolved")
}

func TestServiceTokenRequired_DeniesMissingBearer(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServiceTokenRequired_DeniesNonServiceTokenCredential(t *testing.T) {
	// A JWT or delegated token is not a service token: these service routes accept only service tokens.
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: false}
	r := newServiceTokenTestRouter(resolver, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServiceTokenRequired_NilResolverFailsClosed(t *testing.T) {
	r := newServiceTokenTestRouter(nil, controlplane.PermCreditsWrite)
	w := doServiceTokenRequest(r, true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
