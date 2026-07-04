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

	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// #757 merchant self-serve API keys over the REAL standalone server + AuthKit
// control plane: mint/list/revoke, role scoping (viewer = read-only for LLM
// agents), owner gating, no-escalation, revocation, and cross-merchant isolation.

type mintedKeyResp struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Prefix    string `json:"prefix"`
	CreatedAt string `json:"created_at"`
	Secret    string `json:"secret"`
}

func mintKeyHTTP(t *testing.T, baseURL, token, name, role string) (int, mintedKeyResp, []byte) {
	t.Helper()
	status, body := requestJSON(t, http.MethodPost, baseURL+"/v1/merchant/api-keys", token,
		map[string]any{"name": name, "role": role})
	var out mintedKeyResp
	if status == http.StatusCreated {
		require.NoError(t, json.Unmarshal(body, &out))
	}
	return status, out, body
}

func listKeysHTTP(t *testing.T, baseURL, token string) (int, []map[string]any, []byte) {
	t.Helper()
	status, body := requestJSON(t, http.MethodGet, baseURL+"/v1/merchant/api-keys", token, nil)
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &envelope))
	}
	return status, envelope.Data, body
}

const metricsQueryBody = `{"measures":["cancellations"],"by":["time"],"grain":"day","range":{"last":"7d"}}`

func TestMerchantSelfServeAPIKeys(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	admin := surface.Token // the real bootstrap-minted owner admin key
	base := surface.BaseURL

	t.Run("unauthenticated is rejected", func(t *testing.T) {
		status, _, _ := listKeysHTTP(t, base, "")
		require.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("unknown role is rejected against the fixed catalog", func(t *testing.T) {
		for _, role := range []string{"superadmin", "admin", "member", ""} {
			status, _, body := mintKeyHTTP(t, base, admin, "bad-role-key", role)
			require.Equalf(t, http.StatusBadRequest, status, "role %q: %s", role, string(body))
			require.Contains(t, string(body), "unknown_role")
		}
	})

	var viewerKey mintedKeyResp
	var viewerToken string
	t.Run("owner mints a viewer key; secret shown once with prefix", func(t *testing.T) {
		status, minted, body := mintKeyHTTP(t, base, admin, "llm-agent", "viewer")
		require.Equalf(t, http.StatusCreated, status, "mint: %s", string(body))
		require.NotEmpty(t, minted.ID)
		require.Equal(t, "llm-agent", minted.Name)
		require.Equal(t, "viewer", minted.Role)
		require.NotEmpty(t, minted.Secret)
		require.True(t, strings.HasPrefix(minted.Prefix, "openrails_st_"), "prefix %q", minted.Prefix)
		require.True(t, strings.HasPrefix(minted.Secret, minted.Prefix),
			"the presented token must start with the listed prefix")
		require.NotEmpty(t, minted.CreatedAt)
		viewerKey = minted
		viewerToken = minted.Secret
	})

	t.Run("viewer key CAN read metrics schema and run queries", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodGet, base+"/v1/merchant/metrics/schema", viewerToken, nil)
		require.Equalf(t, http.StatusOK, status, "schema: %s", string(body))
		require.Contains(t, string(body), "measures")

		status, body = requestJSON(t, http.MethodPost, base+"/v1/merchant/metrics/query", viewerToken,
			json.RawMessage(metricsQueryBody))
		require.Equalf(t, http.StatusOK, status, "query: %s", string(body))
	})

	t.Run("viewer key CANNOT refund", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPost,
			base+"/v1/merchant/payments/"+uuid.NewString()+"/refunds", viewerToken,
			map[string]any{"amount": 1000000})
		require.Equalf(t, http.StatusForbidden, status, "refund: %s", string(body))
	})

	t.Run("viewer key CANNOT manage API keys (owner-gated)", func(t *testing.T) {
		status, _, _ := listKeysHTTP(t, base, viewerToken)
		require.Equal(t, http.StatusForbidden, status)
		status, _, _ = mintKeyHTTP(t, base, viewerToken, "escalate", "viewer")
		require.Equal(t, http.StatusForbidden, status)
		status, _ = requestJSON(t, http.MethodDelete, base+"/v1/merchant/api-keys/"+viewerKey.ID, viewerToken, nil)
		require.Equal(t, http.StatusForbidden, status)
	})

	t.Run("list shows metadata but never secret material", func(t *testing.T) {
		status, keys, raw := listKeysHTTP(t, base, admin)
		require.Equal(t, http.StatusOK, status)
		require.NotEmpty(t, keys)
		var found bool
		for _, k := range keys {
			require.NotContains(t, k, "secret")
			require.NotEmpty(t, k["prefix"])
			if k["id"] == viewerKey.ID {
				found = true
				require.Equal(t, "viewer", k["role"])
				require.Equal(t, "llm-agent", k["name"])
			}
		}
		require.True(t, found, "minted key must appear in the list")
		// The secret part after the prefix must not appear ANYWHERE in the payload.
		secretPart := strings.TrimPrefix(viewerToken, viewerKey.Prefix)
		require.NotEmpty(t, secretPart)
		require.NotContains(t, string(raw), secretPart)
	})

	t.Run("cross-merchant isolation", func(t *testing.T) {
		b := surface.ProvisionOwnedMerchant("keyiso" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
		status, bKey, body := mintKeyHTTP(t, base, b.APIKey, "b-integration", "viewer")
		require.Equalf(t, http.StatusCreated, status, "mint under B: %s", string(body))

		// A's list never contains B's key; B's list never contains A's.
		_, aKeys, _ := listKeysHTTP(t, base, admin)
		for _, k := range aKeys {
			require.NotEqual(t, bKey.ID, k["id"])
		}
		_, bKeys, _ := listKeysHTTP(t, base, b.APIKey)
		for _, k := range bKeys {
			require.NotEqual(t, viewerKey.ID, k["id"])
		}

		// A cannot revoke B's key by id (scoped to A's group -> 404).
		status, body = requestJSON(t, http.MethodDelete, base+"/v1/merchant/api-keys/"+bKey.ID, admin, nil)
		require.Equalf(t, http.StatusNotFound, status, "cross-merchant revoke: %s", string(body))
		// ... and B's key still works.
		status, _ = requestJSON(t, http.MethodGet, base+"/v1/merchant/metrics/schema", bKey.Secret, nil)
		require.Equal(t, http.StatusOK, status)
	})

	t.Run("non-owner-covering credential cannot mint above itself", func(t *testing.T) {
		// A delegated token bounded to credentials:manage alone passes the route
		// gate but covers no catalog role's permission set -> every mint is a
		// no-escalation 403. A merchant:*-bounded one mints fine.
		bounded := surface.RegisterDelegatedCaller("apikeymgr"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8],
			dbtest.TestMerchantSlug, uuid.NewString(), []string{controlplane.PermMerchantCredentialsManage})
		for _, role := range []string{"owner", "support", "viewer"} {
			status, _, body := mintKeyHTTP(t, base, bounded.Token, "escalated-"+role, role)
			require.Equalf(t, http.StatusForbidden, status, "role %q: %s", role, string(body))
			require.Contains(t, string(body), "role_escalation")
		}
		full := surface.RegisterDelegatedCaller("apikeyown"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8],
			dbtest.TestMerchantSlug, uuid.NewString(), []string{"merchant:*"})
		status, minted, body := mintKeyHTTP(t, base, full.Token, "delegated-minted", "viewer")
		require.Equalf(t, http.StatusCreated, status, "owner-covering delegated mint: %s", string(body))
		require.NotEmpty(t, minted.Secret)
	})

	t.Run("user sessions: owner user can mint, viewer user cannot", func(t *testing.T) {
		cp := embcp.Get(surface.App())
		require.NotNil(t, cp)
		core := cp.Core()

		mintUserToken := func(role string) string {
			name := "apikeyuser" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
			user, err := core.CreateUser(ctx, name+"@example.com", name)
			require.NoError(t, err)
			require.NoError(t, core.Genesis().AssignGroupRole(ctx, controlplane.MerchantType,
				dbtest.TestMerchantSlug, user.ID, authcore.SubjectKindUser, role))
			token, _, err := core.MintAccessToken(ctx, user.ID, nil)
			require.NoError(t, err)
			return token
		}

		ownerSession := mintUserToken(controlplane.MerchantRoleOwner)
		status, minted, body := mintKeyHTTP(t, base, ownerSession, "console-minted", "viewer")
		require.Equalf(t, http.StatusCreated, status, "owner user mint: %s", string(body))
		require.NotEmpty(t, minted.Secret)

		viewerSession := mintUserToken(controlplane.MerchantRoleViewer)
		status, _, body = mintKeyHTTP(t, base, viewerSession, "console-escalate", "viewer")
		require.Equalf(t, http.StatusForbidden, status, "viewer user mint: %s", string(body))
	})

	t.Run("revoked key is rejected everywhere", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodDelete, base+"/v1/merchant/api-keys/"+viewerKey.ID, admin, nil)
		require.Equalf(t, http.StatusOK, status, "revoke: %s", string(body))
		require.Contains(t, string(body), `"revoked":true`)

		// The revoked key is dead on every surface.
		status, _ = requestJSON(t, http.MethodGet, base+"/v1/merchant/metrics/schema", viewerToken, nil)
		require.Equal(t, http.StatusUnauthorized, status)
		status, _ = requestJSON(t, http.MethodPost, base+"/v1/merchant/metrics/query", viewerToken,
			json.RawMessage(metricsQueryBody))
		require.Equal(t, http.StatusUnauthorized, status)
		status, _, _ = listKeysHTTP(t, base, viewerToken)
		require.Equal(t, http.StatusUnauthorized, status)

		// Second revoke -> 404 (no live key); the list still shows it, revoked.
		status, _ = requestJSON(t, http.MethodDelete, base+"/v1/merchant/api-keys/"+viewerKey.ID, admin, nil)
		require.Equal(t, http.StatusNotFound, status)
		_, keys, _ := listKeysHTTP(t, base, admin)
		var revokedSeen bool
		for _, k := range keys {
			if k["id"] == viewerKey.ID {
				revokedSeen = true
				require.NotEmpty(t, k["revoked_at"], "revoked key stays listed with revoked_at")
			}
		}
		require.True(t, revokedSeen)
	})

	t.Run("resolved key still parses as an authkit token", func(t *testing.T) {
		// Guard the wire format assumption behind Prefix: marker + key id.
		require.True(t, authkit.HasAPIKeyPrefix(controlplane.APIKeyPrefix, viewerToken))
	})
}
