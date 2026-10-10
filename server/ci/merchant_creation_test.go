//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"github.com/stretchr/testify/require"
)

// A user creates a merchant only through a hosted product's own route, on
// ProvisionMerchant with the user as owner (#1173): the creation policy
// (verified email, reserved names, the free allowance) applies, and the
// user's merchants list it with the owner role. OpenRails serves no route
// that creates or lists merchants.
func TestMerchantCreationPolicy(t *testing.T) {
	f := newFixture(t)
	reserved := uniqueName("house")
	cp := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.MerchantCreation = &server.MerchantCreationConfig{ReservedSlugs: []string{reserved}, FreeAllowance: 1}
		deps.HasVaultedPaymentMethod = func(context.Context, string) (bool, error) { return false, nil }
	})
	ctx := t.Context()
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	account := newAccount(t, cp)
	owner, other := account.ID, newAccount(t, cp).ID
	unverified, _ := newUser(t, cp)
	create := func(user, name string) (*billing.ProvisionMerchantResult, error) {
		return cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: name, DisplayName: "Shop One", OwnerUserID: user})
	}
	displayName := func(id billing.MerchantID) *string {
		m, err := cp.GetMerchant(ctx, id)
		require.NoError(t, err)
		return m.DisplayName
	}

	shop := uniqueName("shop")
	_, err = create(unverified, shop)
	require.ErrorIs(t, err, billing.ErrMerchantCreationEmailUnverified, "an unverified account claims no name")

	created, err := create(owner, shop)
	require.NoError(t, err)
	require.True(t, created.Created)
	ownerToken := authtest.SignIn(t, cp.AuthKit(), account).AccessToken
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+ownerToken)
	mine, err := cp.ListUserMerchants(ctx, r)
	require.NoError(t, err)
	require.Equal(t, []billing.UserMerchant{{ID: created.MerchantID, Slug: shop, DisplayName: "Shop One", Role: "owner", Permissions: []string{"merchant:*"}}}, mine)

	repaired, err := create(owner, shop)
	require.NoError(t, err, "an owned name is the idempotent repair, past the allowance")
	require.Equal(t, []any{created.MerchantID, false}, []any{repaired.MerchantID, repaired.Created})
	_, err = create(owner, uniqueName("second"))
	require.ErrorIs(t, err, billing.ErrMerchantCreationPaymentMethodRequired, "past the allowance a vaulted payment method is required")

	taken, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: shop, DisplayName: "Hijacked", OwnerUserID: other})
	require.NoError(t, err)
	require.Equal(t, []any{created.MerchantID, false}, []any{taken.MerchantID, taken.Created}, "a taken name never becomes another merchant")
	require.Equal(t, "Shop One", *displayName(created.MerchantID), "nor takes another user's display name")
	_, err = create(other, reserved)
	require.ErrorIs(t, err, billing.ErrMerchantSlugReserved)
	_, err = create(other, "Not A Name!")
	require.ErrorIs(t, err, billing.ErrInvalidMerchantSlug)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := call(t, handler, ownerToken, method, "/v1/merchants", "", map[string]string{"name": uniqueName("late")})
		require.Equal(t, http.StatusNotFound, w.Code, "%s /v1/merchants: %s", method, w.Body.String())
	}
}
