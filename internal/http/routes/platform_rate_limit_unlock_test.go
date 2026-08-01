package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type platformUnlockAuthenticator struct{}

func (platformUnlockAuthenticator) Authenticate(context.Context, *http.Request) (billingauth.UserContext, error) {
	return billingauth.UserContext{UserID: "22222222-2222-2222-2222-222222222222"}, nil
}

type platformUnlockRootChecker struct {
	permission string
}

func (c *platformUnlockRootChecker) HasRootPermission(_ context.Context, _ string, permission string) (bool, error) {
	c.permission = permission
	return true, nil
}

type platformUnlockRecorder struct {
	userID  string
	actorID string
}

func (r *platformUnlockRecorder) Unlock(_ context.Context, userID, actorID string) error {
	r.userID = userID
	r.actorID = actorID
	return nil
}

func TestPlatformAdminRateLimitUnlock(t *testing.T) {
	mux := http.NewServeMux()
	root := &platformUnlockRootChecker{}
	unlocker := &platformUnlockRecorder{}
	RegisterPlatformRoutes(router.NewMux(mux, "/v1/platform", nil), nil, PlatformOptions{
		Authenticator: platformUnlockAuthenticator{},
		Root:          root,
		AdminLimiter:  unlocker,
	})

	target := "11111111-1111-1111-1111-111111111111"
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodDelete,
		"/v1/platform/admin-rate-limit-lockouts/"+target,
		nil,
	))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, controlplane.PermRootAdminRateLimitsUnlock, root.permission)
	require.Equal(t, target, unlocker.userID)
	require.Equal(t, "22222222-2222-2222-2222-222222222222", unlocker.actorID)
}

func TestPlatformAdminRateLimitUnlockRejectsInvalidTarget(t *testing.T) {
	mux := http.NewServeMux()
	unlocker := &platformUnlockRecorder{}
	RegisterPlatformRoutes(router.NewMux(mux, "/v1/platform", nil), nil, PlatformOptions{
		Authenticator: platformUnlockAuthenticator{},
		Root:          &platformUnlockRootChecker{},
		AdminLimiter:  unlocker,
	})

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodDelete,
		"/v1/platform/admin-rate-limit-lockouts/not-a-uuid",
		nil,
	))

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Empty(t, unlocker.userID)
}
