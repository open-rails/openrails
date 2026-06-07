package ginroutes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/pkg/tenant"
)

// These tests exercise the HTTP MINT route (POST /v1/service/delegated-tokens)
// end to end through the real service token gate + permission check (issue #222 browser
// tier). They use a fake service token resolver to pin the calling tenant + permissions and
// a recording fake minter to assert the load-bearing security invariant: the
// minted token's tenant is bound to the CALLING service token, never the request body.

// fakeServiceTokenResolver implements ginmw.ServiceTokenResolver for the mint route tests.
type fakeServiceTokenResolver struct {
	looksLikeServiceToken bool
	resolved              *controlplane.ResolvedServiceToken
	err                   error
	serviceJWTResolved    *controlplane.ResolvedServiceToken
	serviceJWTErr         error
}

func (f fakeServiceTokenResolver) LooksLikeServiceToken(string) bool { return f.looksLikeServiceToken }
func (f fakeServiceTokenResolver) ResolveServiceToken(context.Context, string) (*controlplane.ResolvedServiceToken, error) {
	return f.resolved, f.err
}
func (f fakeServiceTokenResolver) ResolveServiceJWT(context.Context, string) (*controlplane.ResolvedServiceToken, error) {
	if f.serviceJWTResolved != nil || f.serviceJWTErr != nil {
		return f.serviceJWTResolved, f.serviceJWTErr
	}
	return nil, authcore.ErrInvalidServiceJWT
}

// recordingMinter records the params it was called with and returns a fixed token.
type recordingMinter struct {
	gotParams controlplane.MintDelegatedParams
	called    bool
	retErr    error
}

func (m *recordingMinter) MintDelegatedAccessToken(_ context.Context, p controlplane.MintDelegatedParams) (*controlplane.MintedDelegatedToken, error) {
	m.called = true
	m.gotParams = p
	if m.retErr != nil {
		return nil, m.retErr
	}
	return &controlplane.MintedDelegatedToken{
		Token:     "minted.jwt.token",
		ExpiresAt: time.Unix(1_700_000_000, 0).UTC(),
	}, nil
}

func newMintRouter(t *testing.T, resolver ginmw.ServiceTokenResolver, minter DelegatedMinter) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/service")
	RegisterServiceRoutes(group, nil, ginmw.ServiceTokenRequired(resolver), minter, nil)
	return e
}

func postMint(e *gin.Engine, withAuth bool, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/service/delegated-tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer openrails_st_keyid_secret")
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func doServiceRoute(e *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func operatorServiceToken(org string) *controlplane.ResolvedServiceToken {
	return &controlplane.ResolvedServiceToken{
		AuthKitTenantSlug: org,
		TenantSlug:        org,
		Permissions:       []string{controlplane.PermSelfMint},
	}
}

// Happy path: a service token holding PermSelfMint mints a token; the response carries the
// token + RFC3339 expiry, and the minter was called with the service token's tenant.
func TestMintRoute_SucceedsAndReturnsToken(t *testing.T) {
	minter := &recordingMinter{}
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, resolved: operatorServiceToken("acme-org")}
	e := newMintRouter(t, resolver, minter)

	w := postMint(e, true, `{"delegated_sub":"user-42","permissions":["openrails:self:billing:read"],"ttl_seconds":120}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, "minted.jwt.token", resp.Token)
	require.NotEmpty(t, resp.ExpiresAt)
	_, err := time.Parse(time.RFC3339, resp.ExpiresAt)
	require.NoError(t, err, "expires_at must be RFC3339")

	require.True(t, minter.called)
	require.Equal(t, "acme-org", minter.gotParams.Tenant)
	require.Equal(t, "user-42", minter.gotParams.DelegatedSubject)
	require.Equal(t, []string{"openrails:self:billing:read"}, minter.gotParams.Permissions)
	require.Equal(t, 120*time.Second, minter.gotParams.TTL)
}

// (3) The minted token's tenant ALWAYS equals the service token's tenant: a body-supplied
// `tenant`/`org` is ignored. The handler never reads tenant from the body.
func TestMintRoute_BodyCannotOverrideTenant(t *testing.T) {
	minter := &recordingMinter{}
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, resolved: operatorServiceToken("caller-org")}
	e := newMintRouter(t, resolver, minter)

	// Attacker tries to smuggle a different tenant in the body.
	w := postMint(e, true, `{"delegated_sub":"u1","permissions":["openrails:self:billing:read"],"tenant":"victim-org","org":"victim-org"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "caller-org", minter.gotParams.Tenant, "tenant must come from the service token, not the body")
}

// (5) Calling without a valid service token is rejected: the service token gate runs before the
// handler, so no mint happens.
func TestMintRoute_RejectedWithoutServiceToken(t *testing.T) {
	minter := &recordingMinter{}
	// Resolver reports the credential is not a service token at all.
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: false}
	e := newMintRouter(t, resolver, minter)

	w := postMint(e, false, `{"delegated_sub":"u1","permissions":["openrails:self:billing:read"]}`)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.False(t, minter.called, "no token may be minted without a valid service token")
}

func TestCreditServiceRoutesRejectDelegatedJWTs(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: false}
	e := newMintRouter(t, resolver, nil)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"hold", http.MethodPost, "/v1/service/credits/hold"},
		{"capture-plural", http.MethodPost, "/v1/service/credits/holds/00000000-0000-0000-0000-000000000001/capture"},
		{"release-plural", http.MethodPost, "/v1/service/credits/holds/00000000-0000-0000-0000-000000000001/release"},
		{"capture-singular", http.MethodPost, "/v1/service/credits/hold/00000000-0000-0000-0000-000000000001/capture"},
		{"release-singular", http.MethodPost, "/v1/service/credits/hold/00000000-0000-0000-0000-000000000001/release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doServiceRoute(e, tc.method, tc.path, "eyJ.delegated.jwt", `{}`)
			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.Contains(t, w.Body.String(), "service_jwt_invalid")
		})
	}
}

func TestServiceEntitlementRouteRequiresEntitlementReadPermission(t *testing.T) {
	e := newMintRouter(t, fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "acme-org",
			Permissions:       []string{controlplane.PermCreditsRead},
		},
	}, nil)
	w := doServiceRoute(e, http.MethodGet, "/v1/service/tenant-subjects/not-a-uuid/entitlements", "openrails_st_keyid_secret", "")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_permission_required")

	e = newMintRouter(t, fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "acme-org",
			Permissions:       []string{controlplane.PermEntitlementsRead},
		},
	}, nil)
	w = doServiceRoute(e, http.MethodGet, "/v1/service/tenant-subjects/not-a-uuid/entitlements", "openrails_st_keyid_secret", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "invalid tenant_subject_id")
}

func TestServiceEntitlementRouteAcceptsResolvedServiceJWTPrincipal(t *testing.T) {
	e := newMintRouter(t, fakeServiceTokenResolver{
		looksLikeServiceToken: false,
		serviceJWTResolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "local-stack",
			TenantID:          tenant.DefaultID,
			TenantSlug:        tenant.DefaultSlug,
			Permissions:       []string{controlplane.PermEntitlementsRead},
			Resources:         []authcore.ServiceTokenResource{controlplane.TenantResource(tenant.DefaultID)},
		},
	}, nil)
	w := doServiceRoute(e, http.MethodGet, "/v1/service/tenant-subjects/not-a-uuid/entitlements", "eyJ.service.jwt", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "invalid tenant_subject_id")
}

func TestServiceEntitlementRouteRejectsOldUserPath(t *testing.T) {
	e := newMintRouter(t, fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "acme-org",
			Permissions:       []string{controlplane.PermEntitlementsRead},
		},
	}, nil)
	w := doServiceRoute(e, http.MethodGet, "/v1/service/users/00000000-0000-0000-0000-000000000001/entitlements", "openrails_st_keyid_secret", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestServiceCreditBalanceRouteRejectsWrongTenantSubjectScope(t *testing.T) {
	target := uuid.New()
	other := uuid.New()
	e := newMintRouter(t, fakeServiceTokenResolver{
		looksLikeServiceToken: true,
		resolved: &controlplane.ResolvedServiceToken{
			AuthKitTenantSlug: "acme-org",
			TenantID:          tenant.DefaultID,
			Permissions:       []string{controlplane.PermCreditsRead},
			Resources: []authcore.ServiceTokenResource{
				controlplane.TenantResource(tenant.DefaultID),
				controlplane.TenantSubjectResource(other),
			},
		},
	}, nil)

	w := doServiceRoute(e, http.MethodGet, "/v1/service/credits/balance?tenant_subject_id="+target.String(), "openrails_st_keyid_secret", "")
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "service_token_tenant_subject_scope_denied")
}

// A service token lacking PermSelfMint is forbidden: the per-route permission gate blocks
// it before the handler.
func TestMintRoute_RejectedWithoutMintPermission(t *testing.T) {
	minter := &recordingMinter{}
	serviceToken := &controlplane.ResolvedServiceToken{
		AuthKitTenantSlug: "acme-org",
		Permissions:       []string{controlplane.PermCreditsWrite}, // no self:mint
	}
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, resolved: serviceToken}
	e := newMintRouter(t, resolver, minter)

	w := postMint(e, true, `{"delegated_sub":"u1","permissions":["openrails:self:billing:read"]}`)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, minter.called)
}

// A forbidden (non-self) requested permission surfaces as 403 from the handler.
func TestMintRoute_ForbiddenPermissionRejected(t *testing.T) {
	minter := &recordingMinter{retErr: &controlplane.ErrMintForbiddenPermission{Permission: "openrails:credits:write"}}
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, resolved: operatorServiceToken("acme-org")}
	e := newMintRouter(t, resolver, minter)

	w := postMint(e, true, `{"delegated_sub":"u1","permissions":["openrails:credits:write"]}`)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.True(t, minter.called)
}

// A missing delegated_sub fails request binding (delegated_sub is required).
func TestMintRoute_MissingSubjectRejected(t *testing.T) {
	minter := &recordingMinter{}
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, resolved: operatorServiceToken("acme-org")}
	e := newMintRouter(t, resolver, minter)

	w := postMint(e, true, `{"permissions":["openrails:self:billing:read"]}`)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.False(t, minter.called)
}

// The mint route is not mounted when no minter is wired (verifier-only mode).
func TestMintRoute_NotMountedWithoutMinter(t *testing.T) {
	resolver := fakeServiceTokenResolver{looksLikeServiceToken: true, resolved: operatorServiceToken("acme-org")}
	e := newMintRouter(t, resolver, nil)

	w := postMint(e, true, `{"delegated_sub":"u1","permissions":["openrails:self:billing:read"]}`)
	require.Equal(t, http.StatusNotFound, w.Code)
}
