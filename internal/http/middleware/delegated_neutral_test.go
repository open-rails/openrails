package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakeDelegatedResolver is a programmable DelegatedResolver for the delegated
// self-service middleware tests (issue #222 browser tier; neutral since #670).
type fakeDelegatedResolver struct {
	resolved *controlplane.ResolvedDelegated
	err      error
}

func (f fakeDelegatedResolver) ResolveDelegated(context.Context, string, string) (*controlplane.ResolvedDelegated, error) {
	return f.resolved, f.err
}

func newDelegatedTestRouter(resolver DelegatedResolver, perm string) http.Handler {
	mux := http.NewServeMux()
	mw := []router.Middleware{DelegatedSelfRequired(resolver)}
	if perm != "" {
		mw = append(mw, RequirePermission(perm))
	}
	rr := router.NewMux(mux, "", nil)
	rr.Handle(http.MethodGet, "/self", func(r *request.Request) {
		resolved, _ := DelegatedFromRequest(r)
		tid, _ := merchant.FromContext(r.Request.Context())
		uc, _ := r.UserContext()
		r.JSON(http.StatusOK, map[string]any{
			"merchant":       tid.String(),
			"subject":        resolved.DelegatedSubject,
			"user_id":        uc.UserID,
			"email":          uc.Email,
			"email_verified": uc.EmailVerified,
			"username":       uc.Username,
		})
	}, mw...)
	return mux
}

func doDelegatedRequest(r http.Handler, withAuth bool) *httptest.ResponseRecorder {
	if withAuth {
		return doDelegatedBearerRequest(r, "eyJ.delegated.jwt")
	}
	return doDelegatedBearerRequest(r, "")
}

func doDelegatedBearerRequest(r http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/self", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestDelegatedSelfRequired_SucceedsAndBindsActingUser(t *testing.T) {
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{
			Merchant:         "operator",
			MerchantID:       dbtest.TestMerchantID,
			MerchantSlug:     dbtest.TestMerchantSlug,
			DelegatedSubject: "user-123",
			Email:            "user@example.test",
			EmailVerified:    true,
			Username:         "user123",
		},
	}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusOK, w.Code)
	// Merchant pinned + acting user bound to delegated_sub so reused `me` handlers
	// scope to this user.
	require.Contains(t, w.Body.String(), dbtest.TestMerchantID.String())
	require.Contains(t, w.Body.String(), "user-123")
	require.Contains(t, w.Body.String(), "user@example.test")
	require.Contains(t, w.Body.String(), "user123")
}

func TestDelegatedSelfRequired_DeniesMissingPermission(t *testing.T) {
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{
			Merchant:         "operator",
			MerchantID:       dbtest.TestMerchantID,
			DelegatedSubject: "user-123",
		},
	}
	// Require a merchant-admin scope, which the customer token does not carry.
	r := newDelegatedTestRouter(resolver, controlplane.PermMerchantCustomerSettingsRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "permission_required")
}

func TestDelegatedSelfRequired_NoAdminOverride(t *testing.T) {
	// Delegated browser tokens must NOT cross persona namespaces. A token carrying a
	// foreign-persona apex glob (`root:*`) does not satisfy a concrete merchant
	// permission gate — reach != capability (#567): the glob is namespace-anchored
	// and covers no `merchant:` perm.
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{
			Merchant:         "operator",
			MerchantID:       dbtest.TestMerchantID,
			DelegatedSubject: "user-123",
			Permissions:      []string{"root:*"},
		},
	}
	r := newDelegatedTestRouter(resolver, controlplane.PermMerchantCustomerSettingsRead)
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestDelegatedSelfRequired_DeniesExpired(t *testing.T) {
	resolver := fakeDelegatedResolver{err: authkit.ErrAccessTokenExpired}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_expired")
}

func TestDelegatedSelfRequired_DeniesRevoked(t *testing.T) {
	resolver := fakeDelegatedResolver{err: authkit.ErrAccessTokenRevoked}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_revoked")
}

func TestDelegatedSelfRequired_DeniesNormalSubOrInvalid(t *testing.T) {
	// A normal-`sub` (non-delegated) token, bad audience, or any forbidden
	// permission collapses to the sanitized invalid error from the resolver.
	resolver := fakeDelegatedResolver{err: controlplane.ErrDelegatedInvalid}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_invalid")
}

func TestDelegatedSelfRequired_DeniesServiceCredential(t *testing.T) {
	resolver := fakeDelegatedResolver{err: controlplane.ErrDelegatedInvalid}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedBearerRequest(r, "openrails_st_keyid_secret")
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated_token_invalid")
}

func TestDelegatedSelfRequired_DeniesCrossMerchant(t *testing.T) {
	// Token's merchant maps to no active merchant for this deployment.
	resolver := fakeDelegatedResolver{err: controlplane.ErrServiceCredentialMerchantUnresolved}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "delegated_merchant_unresolved")
}

func TestDelegatedSelfRequired_DeniesOriginMismatch(t *testing.T) {
	resolver := fakeDelegatedResolver{err: controlplane.ErrDelegatedOriginNotAllowed}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), "delegated_origin_not_allowed")
}

func TestDelegatedSelfRequired_DeniesMissingBearer(t *testing.T) {
	resolver := fakeDelegatedResolver{
		resolved: &controlplane.ResolvedDelegated{DelegatedSubject: "user-123"},
	}
	r := newDelegatedTestRouter(resolver, "")
	w := doDelegatedRequest(r, false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "delegated bearer token required")
}

func TestDelegatedSelfRequired_NilResolverFailsClosed(t *testing.T) {
	r := newDelegatedTestRouter(nil, "")
	w := doDelegatedRequest(r, true)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}
