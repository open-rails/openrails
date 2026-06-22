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

// fakeServiceCredentialResolver is a programmable ServiceCredentialResolver for the API key middleware tests
// (issue #222).
type fakeServiceCredentialResolver struct {
	looksLikeAPIKey    bool
	resolved           *controlplane.ResolvedServiceCredential
	err                error
	serviceJWTResolved *controlplane.ResolvedServiceCredential
	serviceJWTErr      error
}

func (f fakeServiceCredentialResolver) LooksLikeAPIKey(string) bool { return f.looksLikeAPIKey }

func (f fakeServiceCredentialResolver) ResolveAPIKey(context.Context, string) (*controlplane.ResolvedServiceCredential, error) {
	return f.resolved, f.err
}

func (f fakeServiceCredentialResolver) ResolveServiceJWT(context.Context, string) (*controlplane.ResolvedServiceCredential, error) {
	if f.serviceJWTResolved != nil || f.serviceJWTErr != nil {
		return f.serviceJWTResolved, f.serviceJWTErr
	}
	return nil, authcore.ErrInvalidServiceJWT
}

func newServiceCredentialTestRouter(resolver ServiceCredentialResolver, perm string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", ServiceCredentialRequired(resolver), RequirePermission(perm), func(c *gin.Context) {
		resolved, _ := ServiceCredentialFromGin(c)
		tid, _ := merchant.FromContext(c.Request.Context())
		c.JSON(http.StatusOK, gin.H{"authkit_org": resolved.OwnerOrgSlug, "merchant": tid.String()})
	})
	return r
}

func doServiceCredentialRequest(r *gin.Engine, withAuth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/svc", nil)
	if withAuth {
		req.Header.Set("Authorization", "Bearer openrails_st_keyid_secret")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireServiceCredentialCustomerScope(t *testing.T) {
	payer := uuid.New()
	resolver := fakeServiceCredentialResolver{
		looksLikeAPIKey: true,
		resolved: &controlplane.ResolvedServiceCredential{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{controlplane.PermMerchantAdmissionsCreate},
			Resources: []authcore.APIKeyResource{
				controlplane.MerchantResource(dbtest.TestMerchantID),
				controlplane.CustomerResource(payer),
			},
		},
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/svc", ServiceCredentialRequired(resolver), func(c *gin.Context) {
		if !RequireServiceCredentialCustomerScope(c, payer) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)

	r = gin.New()
	r.GET("/svc", ServiceCredentialRequired(resolver), func(c *gin.Context) {
		if !RequireServiceCredentialCustomerScope(c, uuid.New()) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	w = doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_credential_customer_scope_denied")
}

func TestServiceCredentialRequired_SucceedsForCorrectMerchantAndPermission(t *testing.T) {
	resolver := fakeServiceCredentialResolver{
		looksLikeAPIKey: true,
		resolved: &controlplane.ResolvedServiceCredential{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			MerchantSlug: dbtest.TestMerchantSlug,
			Permissions:  []string{controlplane.PermMerchantCustomerSettingsUpdate},
		},
	}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), dbtest.TestMerchantID.String())
}

func TestServiceCredentialRequired_SucceedsForServiceJWT(t *testing.T) {
	resolver := fakeServiceCredentialResolver{
		looksLikeAPIKey: false,
		serviceJWTResolved: &controlplane.ResolvedServiceCredential{
			OwnerOrgSlug: "cozy-art",
			MerchantID:   dbtest.TestMerchantID,
			MerchantSlug: dbtest.TestMerchantSlug,
			Permissions:  []string{controlplane.PermMerchantCustomerSettingsUpdate},
		},
	}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), dbtest.TestMerchantID.String())
}

func TestServiceCredentialRequired_ApexGrantDoesNotBypassGate(t *testing.T) {
	resolver := fakeServiceCredentialResolver{
		looksLikeAPIKey: true,
		resolved: &controlplane.ResolvedServiceCredential{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{authcore.OrgOwnerGrant},
		},
	}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestServiceCredentialRequired_DeniesMissingPermission(t *testing.T) {
	resolver := fakeServiceCredentialResolver{
		looksLikeAPIKey: true,
		resolved: &controlplane.ResolvedServiceCredential{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{controlplane.PermMerchantCustomerSettingsRead}, // read only
		},
	}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "permission_required")
}

func TestServiceCredentialRequired_DeniesUnknownPermissionSet(t *testing.T) {
	resolver := fakeServiceCredentialResolver{
		looksLikeAPIKey: true,
		resolved: &controlplane.ResolvedServiceCredential{
			OwnerOrgSlug: "operator",
			MerchantID:   dbtest.TestMerchantID,
			Permissions:  []string{"openrails:something:unknown"},
		},
	}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestServiceCredentialRequired_DeniesExpiredAPIKey(t *testing.T) {
	resolver := fakeServiceCredentialResolver{looksLikeAPIKey: true, err: authcore.ErrAccessTokenExpired}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_credential_expired")
}

func TestServiceCredentialRequired_DeniesRevokedAPIKey(t *testing.T) {
	resolver := fakeServiceCredentialResolver{looksLikeAPIKey: true, err: authcore.ErrAccessTokenRevoked}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_credential_revoked")
}

func TestServiceCredentialRequired_DeniesUnknownAPIKey(t *testing.T) {
	resolver := fakeServiceCredentialResolver{looksLikeAPIKey: true, err: authcore.ErrInvalidAccessToken}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "service_credential_invalid")
}

func TestServiceCredentialRequired_DeniesCrossMerchantAPIKey(t *testing.T) {
	// Owning AuthKit org maps to no active OpenRails merchant for this deployment.
	resolver := fakeServiceCredentialResolver{looksLikeAPIKey: true, err: controlplane.ErrServiceCredentialMerchantUnresolved}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_credential_merchant_unresolved")
}

func TestServiceCredentialRequired_DeniesMissingBearer(t *testing.T) {
	resolver := fakeServiceCredentialResolver{looksLikeAPIKey: true}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServiceCredentialRequired_DeniesNonServiceCredential(t *testing.T) {
	// A JWT or delegated token is not an API key: these service routes accept only API keys.
	resolver := fakeServiceCredentialResolver{looksLikeAPIKey: false}
	r := newServiceCredentialTestRouter(resolver, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestServiceCredentialRequired_NilResolverFailsClosed(t *testing.T) {
	r := newServiceCredentialTestRouter(nil, controlplane.PermMerchantCustomerSettingsUpdate)
	w := doServiceCredentialRequest(r, true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
