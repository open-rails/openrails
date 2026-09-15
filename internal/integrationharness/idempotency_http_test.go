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
	"github.com/stretchr/testify/require"

	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
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
	revoked, err := core.RevokeAPIKey(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), keyID)
	require.NoError(t, err)
	require.True(t, revoked)
	status2, body2, headers2 := doPost(key, bodyJSON)
	require.Equal(t, http.StatusUnauthorized, status2, "revoked credential: %s", body2)
	require.Empty(t, headers2.Get("Idempotent-Replayed"))
}
