package ginmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/authprovider/ginauth"
)

type fakeAdminPrincipalChecker struct {
	allowedOrg  string
	allowedUser string
	allowedPerm string
}

func (f fakeAdminPrincipalChecker) HasAdminPermission(_ context.Context, orgSlug, userID, perm string) (bool, error) {
	return orgSlug == f.allowedOrg && userID == f.allowedUser && perm == f.allowedPerm, nil
}

type fakePlatformPrincipalChecker struct {
	allowedUser string
}

func (f fakePlatformPrincipalChecker) HasPlatformSuperadmin(_ context.Context, userID string) (bool, error) {
	return userID == f.allowedUser, nil
}

func TestUserSessionAdminPrincipalUsesLivePermissionCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("openrails.user_context", ginauth.UserContext{UserID: "admin-1", Org: "merchant-org"})
		c.Next()
	})
	r.GET("/admin", UserSessionAdminPrincipalRequired(fakeAdminPrincipalChecker{
		allowedOrg:  "merchant-org",
		allowedUser: "admin-1",
		allowedPerm: controlplane.PermAdmin,
	}), RequirePermission(controlplane.PermAdmin), func(c *gin.Context) {
		p, ok := PrincipalFromGin(c)
		require.True(t, ok)
		require.Equal(t, CredentialUserSession, p.CredentialType)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestUserSessionAdminPrincipalDeniesNonAdminPermissions(t *testing.T) {
	for _, perm := range []string{controlplane.PermCreditsRead, controlplane.PermSelfBillingRead} {
		t.Run(perm, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("openrails.user_context", ginauth.UserContext{UserID: "admin-1", Org: "merchant-org"})
				c.Next()
			})
			r.GET("/admin", UserSessionAdminPrincipalRequired(fakeAdminPrincipalChecker{
				allowedOrg:  "merchant-org",
				allowedUser: "admin-1",
				allowedPerm: perm,
			}), RequirePermission(perm), func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin", nil))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		})
	}
}

func TestUserSessionPlatformPrincipalDoesNotUseMerchantAdminPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("openrails.user_context", ginauth.UserContext{UserID: "merchant-admin", Org: "merchant-org"})
		c.Next()
	})
	r.GET("/platform", UserSessionPlatformPrincipalRequired(fakePlatformPrincipalChecker{
		allowedUser: "platform-admin",
	}), RequirePermission(controlplane.PermPlatformSuperadmin), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/platform", nil))
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

func TestPrincipalPermissionClassesDoNotBleedAcrossCredentialTypes(t *testing.T) {
	ctx := context.Background()

	servicePrincipal := principalFromServiceCredential(&controlplane.ResolvedServiceToken{
		OwnerOrgSlug: "merchant-org",
		MerchantID:   dbtest.TestMerchantID,
		Permissions: []string{
			controlplane.PermCreditsRead,
			controlplane.PermSelfBillingRead,
		},
	}, CredentialAPIKey)
	require.NotNil(t, servicePrincipal)
	require.Empty(t, servicePrincipal.Subject)
	require.True(t, servicePrincipal.Can(ctx, controlplane.PermCreditsRead))
	require.False(t, servicePrincipal.Can(ctx, controlplane.PermSelfBillingRead))

	delegatedPrincipal := principalFromDelegated(&controlplane.ResolvedDelegated{
		MerchantID:       dbtest.TestMerchantID,
		DelegatedSubject: "user-1",
		Permissions:      []string{controlplane.PermSelfBillingRead, controlplane.PermCreditsRead, controlplane.PermPlatformSuperadmin},
	}, CredentialDelegatedUser)
	require.NotNil(t, delegatedPrincipal)
	require.Equal(t, "user-1", delegatedPrincipal.Subject)
	require.True(t, delegatedPrincipal.Can(ctx, controlplane.PermSelfBillingRead))
	require.False(t, delegatedPrincipal.Can(ctx, controlplane.PermCreditsRead))
	require.False(t, delegatedPrincipal.Can(ctx, controlplane.PermPlatformSuperadmin))
}
