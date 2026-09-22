//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
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
	require.NoError(t, core.OperatorAssignGroupRole(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.UserSubject(userID), controlplane.MerchantRoleOwner))
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
	seedEmailMFA(t, h, userID)
	require.Equal(t, http.StatusUnauthorized, post(enrollment))
	token, _, err := core.MintAccessToken(ctx, userID, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, post(token))
	require.NoError(t, core.OperatorAssignGroupRole(ctx, authkit.RootGroup(), authkit.UserSubject(userID), "owner"))
	platform := surface.BaseURL + "/v1/platform/admin-rate-limit-lockouts/" + userID
	status, body := requestJSON(t, http.MethodDelete, platform, enrollment, nil)
	require.Equal(t, http.StatusUnauthorized, status, string(body))
	status, body = requestJSON(t, http.MethodDelete, platform, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	backupID, _ := makeUser(t, core, "backup"+suffix)
	seedEmailMFA(t, h, backupID)
	require.NoError(t, core.OperatorAssignGroupRole(ctx, authkit.RootGroup(), authkit.UserSubject(backupID), "owner"))
	reason := "test account admission"
	require.NoError(t, core.BanUser(ctx, userID, &reason, nil, userID))
	can, err := core.Can(ctx, authkit.UserSubject(userID), controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.Perm("merchant:members:manage"))
	require.NoError(t, err)
	require.True(t, can, "the current role remains usable through the accepted token lifetime")
	require.Equal(t, http.StatusCreated, post(token))
	status, body = requestJSON(t, http.MethodDelete, platform, token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	// Identity stays stateless, but both merchant and platform authority are live.
	require.NoError(t, core.OperatorUnassignGroupRole(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.UserSubject(userID), controlplane.MerchantRoleOwner))
	require.NoError(t, core.OperatorUnassignGroupRole(ctx, authkit.RootGroup(), authkit.UserSubject(userID), "owner"))
	require.Equal(t, http.StatusForbidden, post(token))
	status, body = requestJSON(t, http.MethodDelete, platform, token, nil)
	require.Equal(t, http.StatusForbidden, status, string(body))
}

// This admission test starts with completed MFA; the actual enrollment workflow
// is exercised by AuthKit's transport suite. Fixture SQL avoids exporting local
// ceremony primitives solely for test setup.
func seedEmailMFA(t *testing.T, h *Harness, userID string) {
	t.Helper()
	_, err := h.sharedPool().Exec(t.Context(), `INSERT INTO profiles.mfa_settings(user_id,enabled) VALUES($1::uuid,true) ON CONFLICT(user_id) DO UPDATE SET enabled=true`, userID)
	require.NoError(t, err)
	_, err = h.sharedPool().Exec(t.Context(), `INSERT INTO profiles.mfa_factors(user_id,method,is_default) VALUES($1::uuid,'email',true)`, userID)
	require.NoError(t, err)
}
