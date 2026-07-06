//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	authcore "github.com/open-rails/authkit/embedded"

	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// #760 merchant team management over the REAL standalone server + AuthKit
// control plane: roster, invite (existing-user direct add), role changes,
// removal, the last-owner invariant, owner-only gating, and cross-merchant
// isolation. The unregistered-email invite-LINK path is registration-posture
// gated; standalone runs registration CLOSED, so it is asserted to 409 here.

type teamMemberResp struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func listTeamHTTP(t *testing.T, base, token string) (int, []teamMemberResp, []byte) {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, base+"/v1/merchant/team", token, nil)
	var env struct {
		Data []teamMemberResp `json:"data"`
	}
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &env))
	}
	return status, env.Data, body
}

func inviteTeamHTTP(t *testing.T, base, token, email, role string) (int, []byte) {
	t.Helper()
	return requestJSON(t, http.MethodPost, base+"/v1/merchant/team/invites", token,
		map[string]any{"email": email, "role": role})
}

// makeUser creates a real AuthKit user (so it can be invited by email) and
// returns its id and email.
func makeUser(t *testing.T, core *authcore.Client, handle string) (string, string) {
	t.Helper()
	email := handle + "@example.com"
	u, err := core.CreateUser(context.Background(), email, handle)
	require.NoError(t, err, "create user %s", handle)
	return u.ID, email
}

func roleOf(members []teamMemberResp, userID string) (string, bool) {
	for _, m := range members {
		if m.UserID == userID {
			return m.Role, true
		}
	}
	return "", false
}

func TestMerchantTeamManagement(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	owner := surface.Token // bootstrap-minted owner admin key (its actor is the
	// filtered `openrailsbootstrap` synthetic owner — never in the roster, so the
	// owner-cleanup in the last-owner subtest never touches it and the key keeps working).
	base := surface.BaseURL

	cp := embcp.Get(surface.App())
	require.NotNil(t, cp)
	core := cp.Core()
	require.NotNil(t, core)

	mintToken := func(userID string) string {
		token, _, err := core.MintAccessToken(ctx, userID, nil)
		require.NoError(t, err)
		return token
	}

	t.Run("unauthenticated is rejected", func(t *testing.T) {
		status, _, _ := listTeamHTTP(t, base, "")
		require.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("roster excludes the synthetic bootstrap actor", func(t *testing.T) {
		// The only owner so far is the bootstrap api-key actor, which is filtered
		// from the human roster — so it starts empty.
		status, members, body := listTeamHTTP(t, base, owner)
		require.Equalf(t, http.StatusOK, status, "list: %s", string(body))
		for _, m := range members {
			require.NotContains(t, m.Email, "openrails-bootstrap")
		}
	})

	aliceID, aliceEmail := makeUser(t, core, "teamalice")
	t.Run("invite an existing user adds them immediately", func(t *testing.T) {
		status, body := inviteTeamHTTP(t, base, owner, aliceEmail, "owner")
		require.Equalf(t, http.StatusCreated, status, "invite: %s", string(body))
		require.Contains(t, string(body), `"added":true`)

		_, members, _ := listTeamHTTP(t, base, owner)
		role, ok := roleOf(members, aliceID)
		require.True(t, ok, "alice must appear in the roster")
		require.Equal(t, "owner", role)
	})

	bobID, bobEmail := makeUser(t, core, "teambob")
	t.Run("invite a second existing user as viewer", func(t *testing.T) {
		status, body := inviteTeamHTTP(t, base, owner, bobEmail, "viewer")
		require.Equalf(t, http.StatusCreated, status, "invite: %s", string(body))
		require.Contains(t, string(body), `"added":true`)
	})

	t.Run("unknown role is rejected against the fixed catalog", func(t *testing.T) {
		for _, role := range []string{"admin", "superadmin", "member", ""} {
			status, body := inviteTeamHTTP(t, base, owner, "whoever@example.com", role)
			require.Equalf(t, http.StatusBadRequest, status, "role %q: %s", role, string(body))
			require.Contains(t, string(body), "unknown_role")
		}
	})

	t.Run("inviting an unregistered email is disabled on locked-down standalone", func(t *testing.T) {
		status, body := inviteTeamHTTP(t, base, owner, "brandnew@example.com", "viewer")
		require.Equalf(t, http.StatusConflict, status, "invite: %s", string(body))
		require.Contains(t, string(body), "invites_disabled")
	})

	t.Run("change a member role", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPatch, base+"/v1/merchant/team/"+bobID, owner,
			map[string]any{"role": "support"})
		require.Equalf(t, http.StatusOK, status, "patch: %s", string(body))

		_, members, _ := listTeamHTTP(t, base, owner)
		role, ok := roleOf(members, bobID)
		require.True(t, ok)
		require.Equal(t, "support", role)
	})

	t.Run("last owner cannot be demoted or removed", func(t *testing.T) {
		// Reduce to alice as the SOLE owner: remove any OTHER owners this merchant
		// carries — the standalone bootstrap admin's own user, plus any owner a
		// sibling test seeded on the shared `test` merchant. Removing them is
		// allowed while alice is also an owner, and leaves alice the last owner.
		// (The filtered `openrailsbootstrap` actor behind `owner` is never in the
		// roster, so it is untouched and the admin key keeps working.)
		_, members, _ := listTeamHTTP(t, base, owner)
		for _, m := range members {
			if m.Role == "owner" && m.UserID != aliceID {
				st, rb := requestJSON(t, http.MethodDelete, base+"/v1/merchant/team/"+m.UserID, owner, nil)
				require.Equalf(t, http.StatusOK, st, "remove co-owner %s: %s", m.UserID, string(rb))
			}
		}
		// alice is now the sole owner.
		status, body := requestJSON(t, http.MethodPatch, base+"/v1/merchant/team/"+aliceID, owner,
			map[string]any{"role": "viewer"})
		require.Equalf(t, http.StatusBadRequest, status, "demote: %s", string(body))
		require.Contains(t, string(body), "last_owner")

		status, body = requestJSON(t, http.MethodDelete, base+"/v1/merchant/team/"+aliceID, owner, nil)
		require.Equalf(t, http.StatusBadRequest, status, "remove: %s", string(body))
		require.Contains(t, string(body), "last_owner")
	})

	carolID, carolEmail := makeUser(t, core, "teamcarol")
	t.Run("with a second owner, the first can be demoted (self-demotion path)", func(t *testing.T) {
		status, body := inviteTeamHTTP(t, base, owner, carolEmail, "owner")
		require.Equalf(t, http.StatusCreated, status, "invite carol: %s", string(body))

		status, body = requestJSON(t, http.MethodPatch, base+"/v1/merchant/team/"+aliceID, owner,
			map[string]any{"role": "support"})
		require.Equalf(t, http.StatusOK, status, "demote with 2 owners: %s", string(body))

		_, members, _ := listTeamHTTP(t, base, owner)
		role, _ := roleOf(members, aliceID)
		require.Equal(t, "support", role)
		_ = carolID
	})

	t.Run("removing a member takes effect", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodDelete, base+"/v1/merchant/team/"+bobID, owner, nil)
		require.Equalf(t, http.StatusOK, status, "remove bob: %s", string(body))

		_, members, _ := listTeamHTTP(t, base, owner)
		_, ok := roleOf(members, bobID)
		require.False(t, ok, "bob must be gone from the roster")
	})

	t.Run("role change on a non-member is 404", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPatch, base+"/v1/merchant/team/"+bobID, owner,
			map[string]any{"role": "viewer"})
		require.Equalf(t, http.StatusNotFound, status, "patch non-member: %s", string(body))
	})

	t.Run("team surface is owner-only (non-owner is forbidden)", func(t *testing.T) {
		// Give a fresh user the viewer role, mint their session, and confirm the
		// whole /team surface is closed to them.
		vID, vEmail := makeUser(t, core, "teamviewer")
		status, _ := inviteTeamHTTP(t, base, owner, vEmail, "viewer")
		require.Equal(t, http.StatusCreated, status)
		viewerTok := mintToken(vID)

		status, _, _ = listTeamHTTP(t, base, viewerTok)
		require.Equal(t, http.StatusForbidden, status)
		status, _ = inviteTeamHTTP(t, base, viewerTok, "x@example.com", "viewer")
		require.Equal(t, http.StatusForbidden, status)
		status, _ = requestJSON(t, http.MethodPatch, base+"/v1/merchant/team/"+aliceID, viewerTok,
			map[string]any{"role": "viewer"})
		require.Equal(t, http.StatusForbidden, status)
		status, _ = requestJSON(t, http.MethodDelete, base+"/v1/merchant/team/"+aliceID, viewerTok, nil)
		require.Equal(t, http.StatusForbidden, status)
	})

	t.Run("owner user session can manage the team", func(t *testing.T) {
		ownerTok := mintToken(carolID) // carol is an owner
		status, _, body := listTeamHTTP(t, base, ownerTok)
		require.Equalf(t, http.StatusOK, status, "owner-session list: %s", string(body))
	})

	t.Run("pending invites list reports posture", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodGet, base+"/v1/merchant/team/invites", owner, nil)
		require.Equalf(t, http.StatusOK, status, "invites: %s", string(body))
		require.Contains(t, string(body), `"invites_enabled":false`)
	})

	t.Run("cross-merchant isolation", func(t *testing.T) {
		b := surface.ProvisionOwnedMerchant("teamiso" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10])
		daveID, daveEmail := makeUser(t, core, "teamdave"+strings.ReplaceAll(uuid.NewString(), "-", "")[:6])
		status, body := inviteTeamHTTP(t, base, b.APIKey, daveEmail, "support")
		require.Equalf(t, http.StatusCreated, status, "invite dave under B: %s", string(body))

		// A's roster never contains dave.
		_, aMembers, _ := listTeamHTTP(t, base, owner)
		_, inA := roleOf(aMembers, daveID)
		require.False(t, inA, "dave must not appear in A's team")

		// A cannot change or remove dave (not a member of A → 404).
		status, _ = requestJSON(t, http.MethodPatch, base+"/v1/merchant/team/"+daveID, owner,
			map[string]any{"role": "viewer"})
		require.Equal(t, http.StatusNotFound, status)
		status, _ = requestJSON(t, http.MethodDelete, base+"/v1/merchant/team/"+daveID, owner, nil)
		require.Equal(t, http.StatusNotFound, status)

		// B still sees dave.
		_, bMembers, _ := listTeamHTTP(t, base, b.APIKey)
		role, inB := roleOf(bMembers, daveID)
		require.True(t, inB, "dave must be in B's team")
		require.Equal(t, "support", role)
	})
}
