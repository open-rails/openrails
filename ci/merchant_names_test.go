//go:build e2e && integration

package ci_test

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
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/server"
)

// newServer builds a standalone server over the fixture's database, minting
// real user tokens over the shared AuthKit schema. edit adjusts the
// configuration.
func (f *fixture) newServer(t *testing.T, edit func(*server.Config, *server.Deps)) *server.Server {
	t.Helper()
	srv, err := f.buildServer(t, edit)
	require.NoError(t, err)
	return srv
}

// authSchema is the schema of the fixture's standalone AuthKit.
func (f *fixture) authSchema() string { return f.schema + "_auth" }

// buildServer is newServer reporting New's error.
func (f *fixture) buildServer(t *testing.T, edit func(*server.Config, *server.Deps)) (*server.Server, error) {
	t.Helper()
	// Each test's AuthKit has its own schema, as its billing tables do: its
	// accounts, and the sign-in limits AuthKit counts per device, are its own.
	cfg := server.Config{Engine: f.config(), LocalSignIn: true, Auth: server.AuthConfig{
		Issuer: "http://127.0.0.1/" + f.schema, AllowMemory: true, AllowMissingSenders: true,
		AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: t.TempDir(),
		Schema: f.authSchema(),
	}}
	deps := server.Deps{Engine: openrails.Deps{Postgres: f.pool}}
	if edit != nil {
		edit(&cfg, &deps)
	}
	srv, err := server.New(t.Context(), cfg, deps)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	return srv, nil
}

// operatorRename renames a merchant as the operator: no rename interval, no
// reserved-name check.
func operatorRename(ctx context.Context, srv *server.Server, id billing.MerchantID, name string) error {
	_, cp := operator.Of(srv)
	_, err := cp.RenameMerchant(ctx, id, name, "", true)
	return err
}

// standaloneHandler is the server's whole HTTP surface, with job producers
// bound as Run binds them.
func standaloneHandler(srv *server.Server) (http.Handler, error) {
	graph, _ := operator.Of(srv)
	if err := graph.Runtime.InitRiver(context.Background()); err != nil {
		return nil, err
	}
	return srv.Handler(), nil
}

// newUser creates an AuthKit user with an unverified email and returns its id
// and a bearer access token.
func newUser(t *testing.T, cp *server.Server) (string, string) {
	t.Helper()
	username := "u" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	u, err := cp.AuthKit().CreateUser(t.Context(), iam.NewUser{Email: username + "@e2e.test", Username: username})
	require.NoError(t, err)
	token, err := cp.AuthKit().MintAccessToken(t.Context(), u.ID, iam.AccessTokenOptions{})
	require.NoError(t, err)
	return u.ID, token.Value
}

// newOwner is a verified account signed in with its password: owner
// operations need a recent sign-in, which a minted token is not.
func newOwner(t *testing.T, cp *server.Server) (string, string) {
	t.Helper()
	u := newAccount(t, cp)
	return u.ID, authtest.SignIn(t, cp.AuthKit(), u).AccessToken
}

// verifyEmail marks the user's email proven, as the system.
func verifyEmail(t *testing.T, cp *server.Server, userID string) {
	t.Helper()
	verified := true
	_, err := cp.AuthKit().UpdateUser(t.Context(), iam.SystemIdentity(), userID, iam.UserUpdate{EmailVerified: &verified})
	require.NoError(t, err)
}

func uniqueName(prefix string) string { return prefix + "-" + uuid.NewString()[:8] }

func reserving(names ...string) func(*server.Config, *server.Deps) {
	return func(cfg *server.Config, _ *server.Deps) {
		cfg.MerchantCreation = &server.MerchantCreationConfig{ReservedSlugs: names}
	}
}

// call sends an authenticated JSON request to the standalone surface.
func call(t *testing.T, handler http.Handler, token, method, path, selector string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	r := httptest.NewRequest(method, path, &payload)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	if selector != "" {
		r.Header.Set("OpenRails-Merchant", selector)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// OpenRails owns merchant names (#1106): claims, forwarding former names,
// reserved names, release on retirement, and AuthKit groups that carry none.
func TestMerchantNamesAreOwnedByOpenRails(t *testing.T) {
	f := newFixture(t)
	reserved := uniqueName("house")
	cp := f.newServer(t, reserving(reserved))
	ctx := t.Context()
	owner, _ := newUser(t, cp)
	other, _ := newUser(t, cp)

	acme := uniqueName("acme")
	created, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: acme, OwnerUserID: owner})
	require.NoError(t, err)
	require.True(t, created.Created)
	group, err := cp.AuthKit().Group(ctx, iam.GroupByID(created.GroupID))
	require.NoError(t, err)
	require.Equal(t, created.MerchantID.String(), group.ID, "the AuthKit group is keyed by the merchant and carries no name")

	again, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: acme, OwnerUserID: other})
	require.NoError(t, err)
	require.False(t, again.Created, "a taken name never becomes another merchant")
	require.Equal(t, created.MerchantID, again.MerchantID)

	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: reserved, OwnerUserID: owner})
	require.ErrorIs(t, err, billing.ErrMerchantSlugReserved)
	platform, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: reserved})
	require.NoError(t, err, "operators claim reserved names")
	require.True(t, platform.Created)

	renamed := uniqueName("acme")
	require.NoError(t, operatorRename(ctx, cp, created.MerchantID, renamed))
	for _, ref := range []string{acme, renamed} {
		mid, current, err := cp.ResolveMerchantForGroup(ctx, ref)
		require.NoError(t, err, ref)
		require.Equal(t, created.MerchantID, mid, "the former name forwards")
		require.Equal(t, renamed, current)
	}
	blocked, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: acme, OwnerUserID: other})
	require.NoError(t, err)
	require.False(t, blocked.Created, "a former name is not claimable by another merchant")
	var pgErr *pgconn.PgError
	_, err = f.pool.Exec(ctx, "INSERT INTO "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" (slug, status) VALUES ($1, 'active')", acme)
	require.ErrorAs(t, err, &pgErr, "the database guards the namespace for every writer")
	require.Equal(t, "merchant_slug_aliases_pkey", pgErr.ConstraintName)
	require.ErrorIs(t, operatorRename(ctx, cp, platform.MerchantID, acme), billing.ErrMerchantNameTaken)
	require.NoError(t, operatorRename(ctx, cp, created.MerchantID, acme), "a merchant takes its own former name back")
	mid, current, err := cp.ResolveMerchantForGroup(ctx, renamed)
	require.NoError(t, err)
	require.Equal(t, []any{created.MerchantID, acme}, []any{mid, current})

	result, err := cp.RetireUnusedMerchant(ctx, created.MerchantID, created.GroupID)
	require.NoError(t, err)
	require.True(t, result.Retired)
	for _, released := range []string{acme, renamed} {
		_, _, err := cp.ResolveMerchantForGroup(ctx, released)
		require.ErrorIs(t, err, billing.ErrMerchantUnresolved, released)
		reclaimed, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: released, OwnerUserID: other})
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
	cp := f.newServer(t, reserving(reserved))
	ctx := t.Context()
	owner, token := newOwner(t, cp)
	shop := uniqueName("shop")
	m, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: shop, OwnerUserID: owner})
	require.NoError(t, err)
	taken := uniqueName("taken")
	_, err = cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: taken})
	require.NoError(t, err)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	do := func(method, path, selector string, body any) *httptest.ResponseRecorder {
		return call(t, handler, token, method, path, selector, body)
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
	require.JSONEq(t, `{"id":"`+m.MerchantID.String()+`","name":"`+next+`"}`, w.Body.String())
	w = rename(shop, uniqueName("shop"))
	require.Equal(t, http.StatusTooManyRequests, w.Code, "the former name still selects the merchant; the interval refuses: %s", w.Body.String())
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	w = do(http.MethodGet, "/v1/merchant/team", shop, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	mid, current, err := cp.ResolveMerchantForGroup(ctx, next)
	require.NoError(t, err)
	require.Equal(t, []any{m.MerchantID, next}, []any{mid, current})
}
