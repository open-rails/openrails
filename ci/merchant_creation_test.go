//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// Users create merchants through OpenRails' own route (#1106): the name claim,
// reserved names, admission and the per-IP/per-user velocity limit.
func TestMerchantCreationRoute(t *testing.T) {
	f := newFixture(t)
	reserved := uniqueName("house")
	cp := f.attachControlPlane(t, func(cfg *openrails.Config, deps *openrails.Deps) {
		cfg.ControlPlane.MerchantCreation = &openrails.MerchantCreationConfig{ReservedSlugs: []string{reserved}, FreeAllowance: 1}
		deps.HasVaultedPaymentMethod = func(context.Context, string) (bool, error) { return false, nil }
	})
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	owner, ownerToken := newUser(t, cp)
	other, otherToken := newUser(t, cp)
	create := func(token string, body map[string]string) (*json.Decoder, int) {
		w := call(t, handler, token, http.MethodPost, "/v1/merchants", "", body)
		return json.NewDecoder(w.Body), w.Code
	}
	code := func(token string, body map[string]string) int {
		_, status := create(token, body)
		return status
	}

	shop := uniqueName("shop")
	require.Equal(t, http.StatusForbidden, code(ownerToken, map[string]string{"name": shop}), "an unverified account claims no name")
	verifyEmail(t, cp, owner)
	verifyEmail(t, cp, other)

	body, status := create(ownerToken, map[string]string{"name": shop, "display_name": "Shop One"})
	require.Equal(t, http.StatusCreated, status)
	var created struct {
		ID      string `json:"id"`
		Slug    string `json:"slug"`
		Created bool   `json:"created"`
	}
	require.NoError(t, body.Decode(&created))
	require.Equal(t, shop, created.Slug)
	require.True(t, created.Created)
	mine, err := cp.ListUserMerchants(t.Context(), owner)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	require.Equal(t, []billing.UserMerchant{{ID: mine[0].ID, Slug: shop, DisplayName: "Shop One", Role: "owner"}}, mine)
	require.Equal(t, created.ID, mine[0].ID.String())
	w := call(t, handler, ownerToken, http.MethodGet, "/v1/merchant/team", shop, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"role":"owner"`)

	body, status = create(ownerToken, map[string]string{"name": shop})
	require.Equal(t, http.StatusOK, status, "re-posting an owned name is the idempotent repair, past the allowance")
	var repaired struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	require.NoError(t, body.Decode(&repaired))
	require.Equal(t, created.ID, repaired.ID)
	require.False(t, repaired.Created)
	require.Equal(t, http.StatusPaymentRequired, code(ownerToken, map[string]string{"name": uniqueName("second")}), "past the allowance a vaulted payment method is required")

	require.Equal(t, http.StatusConflict, code(otherToken, map[string]string{"name": shop}))
	require.Equal(t, http.StatusConflict, code(otherToken, map[string]string{"name": reserved}))
	require.Equal(t, http.StatusBadRequest, code(otherToken, map[string]string{"name": "Not A Name!"}))

	// Seven claims so far from one client IP; the twelfth is the last allowed.
	for range 5 {
		require.Equal(t, http.StatusBadRequest, code(otherToken, map[string]string{"name": "Not A Name!"}))
	}
	w = call(t, handler, otherToken, http.MethodPost, "/v1/merchants", "", map[string]string{"name": uniqueName("late")})
	require.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))
}
