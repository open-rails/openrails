//go:build greenfield && integration

package greenfield_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// attachControlPlane builds a runtime with an attached control plane that
// mints real user tokens over the shared AuthKit schema.
func (f *fixture) attachControlPlane(t *testing.T, opts controlplane.Options) *controlplane.ControlPlane {
	t.Helper()
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool, f.pool))
	rt, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{
			TestMode:          config.CredentialPostureSandbox,
			ProviderWriteMode: config.ProviderWriteModeReadOnly,
			DB:                &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			ReturnOrigins:     []string{"https://greenfield.test"},
		},
		PGXPool: f.pool,
		River:   embed.RiverManagedByOpenRails(f.schema),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	opts.Auth = &hostconfig.AuthConfig{
		Issuer: "http://127.0.0.1/" + f.schema, AllowMemory: true, AllowMissingSenders: true,
		AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: t.TempDir(),
	}
	cp, err := controlplane.Attach(t.Context(), rt, opts)
	require.NoError(t, err)
	return cp
}

// user creates an AuthKit user and returns its id and a bearer access token.
func newUser(t *testing.T, cp *controlplane.ControlPlane) (string, string) {
	t.Helper()
	username := "u" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	u, err := cp.Core().CreateUser(t.Context(), username+"@greenfield.test", username)
	require.NoError(t, err)
	token, _, err := cp.Core().MintAccessToken(t.Context(), u.ID, nil)
	require.NoError(t, err)
	return u.ID, token
}

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

// OpenRails owns merchant names (#1106): claims, forwarding former names,
// reserved names, release on retirement, and AuthKit groups that carry none.
func TestMerchantNamesAreOwnedByOpenRails(t *testing.T) {
	f := newFixture(t)
	reserved := uniqueName("house")
	cp := f.attachControlPlane(t, controlplane.Options{MerchantCreation: &controlplane.MerchantCreationConfig{ReservedSlugs: []string{reserved}}})
	ctx := t.Context()
	owner, _ := newUser(t, cp)
	other, _ := newUser(t, cp)

	acme := uniqueName("acme")
	created, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: acme, OwnerUserID: owner})
	require.NoError(t, err)
	require.True(t, created.Created)
	group, err := cp.Core().GroupInstanceByID(ctx, created.GroupID)
	require.NoError(t, err)
	require.Equal(t, created.MerchantID.String(), group.InstanceSlug, "the AuthKit group carries no merchant name")

	again, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: acme, OwnerUserID: other})
	require.NoError(t, err)
	require.False(t, again.Created, "a taken name never becomes another merchant")
	require.Equal(t, created.MerchantID, again.MerchantID)

	_, err = cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: reserved, OwnerUserID: owner})
	require.ErrorIs(t, err, controlplane.ErrSlugReserved)
	platform, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: reserved})
	require.NoError(t, err, "operators claim reserved names")
	require.True(t, platform.Created)

	renamed := uniqueName("acme")
	require.NoError(t, cp.RenameMerchant(ctx, created.MerchantID, renamed))
	for _, ref := range []string{acme, renamed} {
		mid, current, err := cp.ResolveMerchantForGroup(ctx, ref)
		require.NoError(t, err, ref)
		require.Equal(t, created.MerchantID, mid, "the former name forwards")
		require.Equal(t, renamed, current)
	}
	blocked, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: acme, OwnerUserID: other})
	require.NoError(t, err)
	require.False(t, blocked.Created, "a former name is not claimable by another merchant")
	var pgErr *pgconn.PgError
	_, err = f.pool.Exec(ctx, "INSERT INTO "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" (slug) VALUES ($1)", acme)
	require.ErrorAs(t, err, &pgErr, "the database guards the namespace for every writer")
	require.Equal(t, "merchant_slug_aliases_pkey", pgErr.ConstraintName)
	require.ErrorIs(t, cp.RenameMerchant(ctx, platform.MerchantID, acme), controlplane.ErrMerchantNameTaken)
	require.NoError(t, cp.RenameMerchant(ctx, created.MerchantID, acme), "a merchant takes its own former name back")
	mid, current, err := cp.ResolveMerchantForGroup(ctx, renamed)
	require.NoError(t, err)
	require.Equal(t, []any{created.MerchantID, acme}, []any{mid, current})

	result, err := cp.RetireUnusedMerchant(ctx, created.MerchantID, created.GroupID)
	require.NoError(t, err)
	require.True(t, result.Retired)
	for _, released := range []string{acme, renamed} {
		_, _, err := cp.ResolveMerchantForGroup(ctx, released)
		require.ErrorIs(t, err, controlplane.ErrMerchantUnresolved, released)
		reclaimed, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: released, OwnerUserID: other})
		require.NoError(t, err)
		require.True(t, reclaimed.Created, "retirement releases the name and its former names")
		require.NotEqual(t, created.MerchantID, reclaimed.MerchantID)
	}
}

// Merchants rename themselves over HTTP under the naming policy; AuthKit's
// group-name routes are not served.
func TestMerchantRenameRoute(t *testing.T) {
	f := newFixture(t)
	reserved := uniqueName("house")
	cp := f.attachControlPlane(t, controlplane.Options{MerchantCreation: &controlplane.MerchantCreationConfig{ReservedSlugs: []string{reserved}}})
	ctx := t.Context()
	owner, token := newUser(t, cp)
	shop := uniqueName("shop")
	m, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: shop, OwnerUserID: owner})
	require.NoError(t, err)
	taken := uniqueName("taken")
	_, err = cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: taken})
	require.NoError(t, err)
	handler, err := cp.Handler()
	require.NoError(t, err)

	do := func(method, path, selector string, body any) *httptest.ResponseRecorder {
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		r := httptest.NewRequest(method, path, &payload)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if selector != "" {
			r.Header.Set("X-OpenRails-Merchant", selector)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	rename := func(selector, to string) *httptest.ResponseRecorder {
		return do(http.MethodPut, "/v1/merchant/name", selector, map[string]string{"name": to})
	}

	require.Equal(t, http.StatusConflict, rename(shop, reserved).Code)
	require.Equal(t, http.StatusConflict, rename(shop, taken).Code)
	require.Equal(t, http.StatusBadRequest, rename(shop, "Not A Name").Code)
	next := uniqueName("shop")
	w := rename(shop, next)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"id":"`+m.MerchantID.String()+`","slug":"`+next+`"}`, w.Body.String())
	w = rename(shop, uniqueName("shop"))
	require.Equal(t, http.StatusTooManyRequests, w.Code, "the former name still selects the merchant; the interval refuses: %s", w.Body.String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	w = do(http.MethodGet, "/v1/merchant/team", shop, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		w := do(method, "/auth/merchant/"+m.GroupID, "", map[string]string{"slug": "hijack"})
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, w.Code, method)
	}
	mid, current, err := cp.ResolveMerchantForGroup(ctx, next)
	require.NoError(t, err)
	require.Equal(t, []any{m.MerchantID, next}, []any{mid, current})
}
