//go:build integration

package integrationharness

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"

	embcp "github.com/open-rails/openrails/internal/operator"
)

// Repeating an HTTP key must re-enter current authorization. Only documented
// financial operations, not credential/admin responses, retain receipts.
func TestHTTPRetriesRecheckAuthority(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	owner := surface.Token
	base := surface.BaseURL

	cp := embcp.Get(surface.App())
	require.NotNil(t, cp)
	core := cp.Core()
	require.NotNil(t, core)

	_, email := makeUser(t, core, "idemphttp"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])

	doPost := func(idemKey, bodyJSON string) (int, []byte, http.Header) {
		req, err := http.NewRequest(http.MethodPost, base+"/v1/merchant/team/invites", strings.NewReader(bodyJSON))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Idempotency-Key", idemKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, raw, resp.Header
	}

	key := uuid.NewString() // unique per run: sibling tests share the package Redis.
	bodyJSON := `{"email":"` + email + `","role":"viewer"}`

	status1, body1, headers1 := doPost(key, bodyJSON)
	require.Equalf(t, http.StatusCreated, status1, "first invite: %s", string(body1))
	require.Empty(t, headers1.Get("Idempotent-Replayed"), "the first (originating) call must not be marked replayed")

	keyID, _, ok := authkit.ParseAPIKey(controlplane.APIKeyPrefix, owner)
	require.True(t, ok)
	keys, err := core.ListAPIKeys(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug))
	require.NoError(t, err)
	var tokenID string
	for _, key := range keys {
		if key.KeyID == keyID {
			tokenID = key.ID
		}
	}
	require.NotEmpty(t, tokenID)
	revoked, err := core.RevokeAPIKey(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), tokenID)
	require.NoError(t, err)
	require.True(t, revoked)
	status2, body2, headers2 := doPost(key, bodyJSON)
	require.Equal(t, http.StatusUnauthorized, status2, "revoked credential: %s", body2)
	require.Empty(t, headers2.Get("Idempotent-Replayed"))
}

func TestHTTPRetriesRecheckMerchantSelector(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	cp := embcp.Get(surface.App())
	core := cp.Core()
	otherSlug := "replay-b-" + uuid.NewString()[:8]
	surface.ProvisionOwnedMerchant(otherSlug)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	actorID, _ := makeUser(t, core, "selector"+suffix)
	for _, slug := range []string{dbtest.TestMerchantSlug, otherSlug} {
		require.NoError(t, core.AdminAssignGroupRole(ctx, controlplane.MerchantGroup(slug), authkit.UserSubject(actorID), controlplane.MerchantRoleOwner))
	}
	token, _, err := core.MintAccessToken(ctx, actorID, nil)
	require.NoError(t, err)
	invitee, email := makeUser(t, core, "selected"+suffix)
	key := uuid.NewString()
	for _, slug := range []string{dbtest.TestMerchantSlug, otherSlug} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+"/v1/merchant/team/invites", strings.NewReader(`{"email":"`+email+`","role":"viewer"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set(billingauth.MerchantSelectorHeader, slug)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, string(raw))
		require.Empty(t, resp.Header.Get("Idempotent-Replayed"))
	}
	groups, err := core.ListSubjectGroups(ctx, authkit.UserSubject(invitee))
	require.NoError(t, err)
	var memberships []string
	for _, group := range groups {
		if group.Persona == controlplane.MerchantType {
			memberships = append(memberships, group.InstanceSlug)
		}
	}
	require.ElementsMatch(t, []string{dbtest.TestMerchantSlug, otherSlug}, memberships)
}
