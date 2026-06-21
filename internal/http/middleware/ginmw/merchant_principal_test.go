package ginmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

// newMerchantPrincipalRouter mounts MerchantPrincipalRequired + a
// RequirePermission(perm) gate + a 200 handler that echoes the resolved
// principal's credential type and subject, so a single test can assert that EVERY
// credential type normalizes to the same gate.
func newMerchantPrincipalRouter(svc ServiceCredentialResolver, del DelegatedResolver, chk AdminPermissionChecker, perm string, withUser *ginauth.UserContext) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if withUser != nil {
		uc := *withUser
		r.Use(func(c *gin.Context) {
			c.Set("openrails.user_context", uc)
			c.Next()
		})
	}
	r.GET("/m", MerchantPrincipalRequired(svc, del, chk), RequirePermission(perm), func(c *gin.Context) {
		if p, ok := PrincipalFromGin(c); ok {
			c.Header("X-Cred", string(p.CredentialType))
			c.Header("X-Sub", p.Subject)
		}
		c.Status(http.StatusOK)
	})
	return r
}

func merchantPrincipalRequest(r *gin.Engine, token string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/m", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	r.ServeHTTP(w, req)
	return w
}

// TestMerchantPrincipalRequired_APIKey: a brand-prefixed API key resolves to a
// service-credential principal and passes the merchant: gate.
func TestMerchantPrincipalRequired_APIKey(t *testing.T) {
	svc := fakeServiceCredentialResolver{
		looksLikeAPIKey: true,
		resolved: &controlplane.ResolvedServiceCredential{
			MerchantID:  dbtest.TestMerchantID,
			Permissions: []string{controlplane.PermMerchantCustomersRead},
		},
	}
	r := newMerchantPrincipalRouter(svc, fakeDelegatedResolver{}, nil, controlplane.PermMerchantCustomersRead, nil)
	w := merchantPrincipalRequest(r, "openrails_st_key")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, string(CredentialAPIKey), w.Header().Get("X-Cred"))
}

// TestMerchantPrincipalRequired_DelegatedJWT: a JWT that is not a programmatic
// credential falls through to the delegated browser resolver.
func TestMerchantPrincipalRequired_DelegatedJWT(t *testing.T) {
	svc := fakeServiceCredentialResolver{looksLikeAPIKey: false} // JWT, ServiceJWT resolve fails
	del := fakeDelegatedResolver{resolved: &controlplane.ResolvedDelegated{
		Merchant:         "operator",
		MerchantID:       dbtest.TestMerchantID,
		DelegatedSubject: "user-1",
		Permissions:      []string{controlplane.PermMerchantCustomersRead},
	}}
	r := newMerchantPrincipalRouter(svc, del, nil, controlplane.PermMerchantCustomersRead, nil)
	w := merchantPrincipalRequest(r, "aaa.bbb.ccc")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, string(CredentialDelegatedUser), w.Header().Get("X-Cred"))
	require.Equal(t, "user-1", w.Header().Get("X-Sub"))
}

// TestMerchantPrincipalRequired_UserSession: with no bearer token, a live user
// session is authorized by live org permissions.
func TestMerchantPrincipalRequired_UserSession(t *testing.T) {
	chk := fakeAdminPrincipalChecker{allowedOrg: "merchant-org", allowedUser: "admin-1", allowedPerm: controlplane.PermMerchantCustomersRead}
	uc := ginauth.UserContext{UserID: "admin-1", Org: "merchant-org"}
	r := newMerchantPrincipalRouter(nil, nil, chk, controlplane.PermMerchantCustomersRead, &uc)
	w := merchantPrincipalRequest(r, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, string(CredentialUserSession), w.Header().Get("X-Cred"))
}

// TestMerchantPrincipalRequired_APIKeyFailureDoesNotFallThrough: a failed API key
// is rejected as-is — it must NEVER fall through to the delegated path (which here
// would otherwise succeed), so a bad API key can't be laundered into a browser grant.
func TestMerchantPrincipalRequired_APIKeyFailureDoesNotFallThrough(t *testing.T) {
	svc := fakeServiceCredentialResolver{looksLikeAPIKey: true, err: authcore.ErrInvalidAccessToken}
	del := fakeDelegatedResolver{resolved: &controlplane.ResolvedDelegated{
		MerchantID:       dbtest.TestMerchantID,
		DelegatedSubject: "x",
		Permissions:      []string{controlplane.PermMerchantCustomersRead},
	}}
	r := newMerchantPrincipalRouter(svc, del, nil, controlplane.PermMerchantCustomersRead, nil)
	w := merchantPrincipalRequest(r, "openrails_st_bad")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// TestMerchantPrincipalRequired_NoCredential: no bearer and no user session = 401.
func TestMerchantPrincipalRequired_NoCredential(t *testing.T) {
	r := newMerchantPrincipalRouter(fakeServiceCredentialResolver{}, fakeDelegatedResolver{}, nil, controlplane.PermMerchantCustomersRead, nil)
	w := merchantPrincipalRequest(r, "")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}
