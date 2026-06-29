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
	allowedMerchant string
	allowedUser     string
	allowedPerm     string
}

func (f fakeAdminPrincipalChecker) HasAdminPermission(_ context.Context, merchantRef, userID, perm string) (bool, error) {
	return merchantRef == f.allowedMerchant && userID == f.allowedUser && perm == f.allowedPerm, nil
}

func TestUserSessionAdminPrincipalUsesLivePermissionCheck(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("openrails.user_context", ginauth.UserContext{UserID: "admin-1", Merchant: "merchant-a"})
		c.Next()
	})
	r.GET("/admin", UserSessionAdminPrincipalRequired(fakeAdminPrincipalChecker{
		allowedMerchant: "merchant-a",
		allowedUser:     "admin-1",
		allowedPerm:     controlplane.PermMerchantCustomerSettingsRead,
	}), RequirePermission(controlplane.PermMerchantCustomerSettingsRead), func(c *gin.Context) {
		p, ok := PrincipalFromGin(c)
		require.True(t, ok)
		require.Equal(t, CredentialUserSession, p.CredentialType)
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestUserSessionAdminPrincipalDeniesNonMerchantPermissions(t *testing.T) {
	for _, perm := range []string{"platform:merchants:update"} {
		t.Run(perm, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("openrails.user_context", ginauth.UserContext{UserID: "admin-1", Merchant: "merchant-a"})
				c.Next()
			})
			r.GET("/admin", UserSessionAdminPrincipalRequired(fakeAdminPrincipalChecker{
				allowedMerchant: "merchant-a",
				allowedUser:     "admin-1",
				allowedPerm:     perm,
			}), RequirePermission(perm), func(c *gin.Context) {
				c.Status(http.StatusOK)
			})

			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin", nil))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		})
	}
}

func TestPrincipalPermissionClassesDoNotBleedAcrossCredentialTypes(t *testing.T) {
	ctx := context.Background()

	servicePrincipal := principalFromServiceCredential(&controlplane.ResolvedServiceCredential{
		OwnerGroupRef: "merchant-a",
		MerchantID:    dbtest.TestMerchantID,
		Permissions: []string{
			controlplane.PermMerchantCustomerSettingsRead,
		},
	}, CredentialAPIKey)
	require.NotNil(t, servicePrincipal)
	require.Empty(t, servicePrincipal.Subject)
	require.True(t, servicePrincipal.Can(ctx, controlplane.PermMerchantCustomerSettingsRead))

	delegatedPrincipal := principalFromDelegated(&controlplane.ResolvedDelegated{
		MerchantID:       dbtest.TestMerchantID,
		DelegatedSubject: "user-1",
		Permissions:      []string{controlplane.PermMerchantCustomerSettingsRead, controlplane.PermMerchantAdmissionsCreate},
	}, CredentialDelegatedUser)
	require.NotNil(t, delegatedPrincipal)
	require.Equal(t, "user-1", delegatedPrincipal.Subject)
	require.True(t, delegatedPrincipal.Can(ctx, controlplane.PermMerchantCustomerSettingsRead))
	require.True(t, delegatedPrincipal.Can(ctx, controlplane.PermMerchantAdmissionsCreate))
	require.False(t, delegatedPrincipal.Can(ctx, "platform:merchants:update"))
}
