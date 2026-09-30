//go:build greenfield && integration

package greenfield_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed/controlplane"
)

// GET /v1/merchants is the console's merchant list (#1106): the live merchants
// a user holds a role in, by current name, with the user's role in each.
func TestUserMerchantsListing(t *testing.T) {
	f := newFixture(t)
	cp := f.attachControlPlane(t, reserving())
	ctx := t.Context()
	handler, err := cp.Handler()
	require.NoError(t, err)
	member, memberToken := newUser(t, cp)
	owner, ownerToken := newUser(t, cp)
	verifyEmail(t, cp, member)
	u, err := cp.Core().User(ctx, iam.UserByID(member))
	require.NoError(t, err)

	own := uniqueName("a-own")
	mine, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: own, OwnerUserID: member})
	require.NoError(t, err)
	require.NoError(t, cp.SetMerchantDisplayName(ctx, mine.MerchantID, "Own Shop"))
	viewed := uniqueName("b-viewed")
	theirs, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: viewed, OwnerUserID: owner})
	require.NoError(t, err)
	w := call(t, handler, ownerToken, http.MethodPost, "/v1/merchant/team/invites", viewed, map[string]string{"email": u.Email, "role": "viewer"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	_, err = cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: uniqueName("c-unrelated"), OwnerUserID: owner})
	require.NoError(t, err)

	list := func(token string) []controlplane.UserMerchant {
		w := call(t, handler, token, http.MethodGet, "/v1/merchants", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var body struct {
			Object string                      `json:"object"`
			Data   []controlplane.UserMerchant `json:"data"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&body))
		require.Equal(t, "list", body.Object)
		return body.Data
	}
	require.Equal(t, []controlplane.UserMerchant{
		{ID: mine.MerchantID, Slug: own, DisplayName: "Own Shop", Role: "owner"},
		{ID: theirs.MerchantID, Slug: viewed, Role: "viewer"},
	}, list(memberToken))

	renamed := uniqueName("a-renamed")
	require.NoError(t, cp.RenameMerchant(ctx, mine.MerchantID, renamed))
	require.Equal(t, renamed, list(memberToken)[0].Slug, "the list carries current names")
	result, err := cp.RetireUnusedMerchant(ctx, mine.MerchantID, mine.GroupID)
	require.NoError(t, err)
	require.True(t, result.Retired)
	require.Equal(t, []controlplane.UserMerchant{{ID: theirs.MerchantID, Slug: viewed, Role: "viewer"}}, list(memberToken))

	w = call(t, handler, "not-a-token", http.MethodGet, "/v1/merchants", "", nil)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
