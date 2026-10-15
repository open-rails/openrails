package routes

import (
	"net/http"
	"testing"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/stretchr/testify/require"
)

func TestAccessTenureSelfServiceAuthorization(t *testing.T) {
	fake := &authtest.Fake{}
	handler := customerSurface(fake)
	path := "/v1/me/access-tenure?entitlement=premium"
	require.Equal(t, http.StatusUnauthorized, reach(handler, http.MethodGet, path, nil))
	token := fake.Person(userA)
	require.True(t, passedGates(reach(handler, http.MethodGet, path, map[string]string{"Authorization": "Bearer " + token})))
	delegated := authtest.Application(userB)
	delegated.Invoker = billingauth.Invoker{Issuer: "https://issuer.test", ID: userA}
	token = fake.Issue(authtest.Grant{Identity: delegated})
	require.Equal(t, http.StatusForbidden, reach(handler, http.MethodGet, path, map[string]string{"Authorization": "Bearer " + token}))
}
