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
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
	"github.com/open-rails/openrails/pkg/tenant"
)

// fakeDelegatedResolver is a programmable DelegatedResolver for the delegated
// self-service middleware tests (issue #222 browser tier).
type fakeDelegatedResolver struct {
	resolved *controlplane.ResolvedDelegated
	err      error
}

func (f fakeDelegatedResolver) ResolveDelegated(context.Context, string) (*controlplane.ResolvedDelegated, error) {
	return f.resolved, f.err
}

func newDelegatedTestRouter(resolver DelegatedResolver, perm string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/self", DelegatedSelfRequired(resolver), RequireDelegatedPermission(perm), func(c *gin.Context) {
		resolved, _ := DelegatedFromGin(c)
		tid, _ := tenant.FromContext(c.Request.Context())
		uc, _ := ginauth.UserContextFromGin(c)
		c.JSON(http.StatusOK, gin.H{
			"tenant":         tid.String(),
			"subject":        resolved.DelegatedSubject,
			"user_id":        uc.UserID,
			"email":          uc.Email,
			"email_verified": uc.EmailVerified,
			"username":       uc.Username,
		})
	})
	return r
}

func doDelegatedRequest(r *gin.Engine, withAuth bool) *httptest.ResponseRecorder {
	if withAuth {
		return doDelegatedBearerRequest(r, "eyJ.delegated.jwt")
	}
	return doDelegatedBearerRequest(r, "")
}

func doDelegatedBearerRequest(r *gin.Engine, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/self", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDelegatedSelfRequired_SucceedsAndBindsActingUser(t *testing.T) {
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{
			Tenant:           "operator",
			TenantID:         dbtest.TestTenantID,
			TenantSlug:       dbtest.TestTenantSlug,
			DelegatedSubject: "user-123",
			Permissions:      []string{controlplane.PermSelfBillingRead},
			Email:            "user@example.test",
			EmailVerified:    true,
			Username:         "user123",
		},
	}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	// Tenant pinned + acting user bound to delegated_sub so reused `me` handlers
	// scope to this user.
	require.Contains(t, w.Body.String(), dbtest.TestTenantID.String())
	require.Contains(t, w.Body.String(), "user-123")
	require.Contains(t, w.Body.String(), "user@example.test")
	require.Contains(t, w.Body.String(), "user123")
}

func TestDelegatedSelfRequired_DeniesMissingPermission(t *testing.T) {
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{
			Tenant:           "operator",
			TenantID:         dbtest.TestTenantID,
			DelegatedSubject: "user-123",
			Permissions:      []string{controlplane.PermSelfBillingRead}, // read only
		},
	}
	// Require the cancel scope, which the token does not carry.
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfSubscriptionCancel)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "delegated_permission_required")
}

func TestDelegatedSelfRequired_NoAdminOverride(t *testing.T) {
	// Self tokens must NOT honor a broad operator/admin grant. A token carrying
	// openrails:admin (which should never happen — verify rejects it — but is
	// asserted here for the gate itself) does not satisfy a self permission gate.
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{
			Tenant:           "operator",
			TenantID:         dbtest.TestTenantID,
			DelegatedSubject: "user-123",
			Permissions:      []string{controlplane.PermAdmin},
		},
	}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestDelegatedSelfRequired_DeniesExpired(t *testing.T) {
	resolver := fakeDelegatedResolver{err: authcore.ErrAccessTokenExpired}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_expired")
}

func TestDelegatedSelfRequired_DeniesRevoked(t *testing.T) {
	resolver := fakeDelegatedResolver{err: authcore.ErrAccessTokenRevoked}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_revoked")
}

func TestDelegatedSelfRequired_DeniesNormalSubOrInvalid(t *testing.T) {
	// A normal-`sub` (non-delegated) token, bad audience, or any forbidden
	// permission collapses to the sanitized invalid error from the resolver.
	resolver := fakeDelegatedResolver{err: controlplane.ErrDelegatedInvalid}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_invalid")
}

func TestDelegatedSelfRequired_DeniesServiceTokenCredential(t *testing.T) {
	resolver := fakeDelegatedResolver{err: controlplane.ErrDelegatedInvalid}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedBearerRequest(r, "openrails_st_keyid_secret")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_invalid")
}

func TestDelegatedSelfRequired_DeniesCrossTenant(t *testing.T) {
	// Token's tenant maps to no active tenant for this deployment.
	resolver := fakeDelegatedResolver{err: controlplane.ErrServiceTokenTenantUnresolved}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "delegated_tenant_unresolved")
}

func TestDelegatedSelfRequired_DeniesMissingBearer(t *testing.T) {
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{DelegatedSubject: "user-123"},
	}
	r := newDelegatedTestRouter(resolver, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated bearer token required")
}

func TestDelegatedSelfRequired_NilResolverFailsClosed(t *testing.T) {
	r := newDelegatedTestRouter(nil, controlplane.PermSelfBillingRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
