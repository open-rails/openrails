//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	"github.com/stretchr/testify/require"
)

func TestHTTPUserAdmissionWorkflow(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	cp := embcp.Get(surface.App())
	core := cp.Core()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	userID, _ := makeUser(t, core, "admission"+suffix)
	require.NoError(t, core.Genesis().AssignGroupRole(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.UserSubject(userID), controlplane.MerchantRoleOwner))
	enrollment, _, err := core.MintAccessToken(ctx, userID, map[string]any{"2fa_enrollment": true})
	require.NoError(t, err)
	_, email := makeUser(t, core, "invitee"+suffix)
	invite := map[string]any{"email": email, "role": "viewer"}
	post := func(token string) int {
		status, body := requestJSON(t, http.MethodPost, surface.BaseURL+"/v1/merchant/team/invites", token, invite)
		t.Logf("merchant mutation status %d: %s", status, body)
		return status
	}
	require.Equal(t, http.StatusUnauthorized, post(enrollment))
	// Completing MFA never upgrades the already-issued enrollment credential.
	_, err = core.Enable2FA(ctx, userID, "email", nil, authcore.AllowAdditionalFactors)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, post(enrollment))
	token, _, err := core.MintAccessToken(ctx, userID, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, post(token))
	require.NoError(t, core.Genesis().AssignRoleBySlug(ctx, userID, "owner"))
	platform := surface.BaseURL + "/v1/platform/admin-rate-limit-lockouts/" + userID
	status, body := requestJSON(t, http.MethodDelete, platform, enrollment, nil)
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	status, body = requestJSON(t, http.MethodDelete, platform, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	backupID, _ := makeUser(t, core, "backup"+suffix)
	_, err = core.Enable2FA(ctx, backupID, "email", nil, authcore.AllowAdditionalFactors)
	require.NoError(t, err)
	require.NoError(t, core.Genesis().AssignRoleBySlug(ctx, backupID, "owner"))
	reason := "test account admission"
	require.NoError(t, core.BanUser(ctx, userID, &reason, nil, userID))
	can, err := core.Can(ctx, authkit.UserSubject(userID), controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.Perm("merchant:members:manage"))
	require.NoError(t, err)
	require.True(t, can, "the role persists; the live account gate must veto it")
	require.Equal(t, http.StatusUnauthorized, post(token))
	status, body = requestJSON(t, http.MethodDelete, platform, token, nil)
	require.Equal(t, http.StatusUnauthorized, status, string(body))
}
