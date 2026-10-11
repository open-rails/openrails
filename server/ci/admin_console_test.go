//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/open-rails/authkit"
	authkittest "github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/server"
)

// consoleBuild stands in for web/admin's build (not built for go test): the
// <base href> placeholder plus one hashed asset.
func consoleBuild(marker string) fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(`<!doctype html><head><base href="/admin/" /><script type="module" src="./assets/index-1a2b3c.js"></script></head>` + marker)},
		"assets/index-1a2b3c.js": {Data: []byte("console.log('console')")},
	}
}

func get(handler http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// requireConsoleAt proves the console is served at mount: index.html (also
// for client routes) carries <base href="mount/">, and config.json and the
// hashed asset resolve beneath it.
func requireConsoleAt(t *testing.T, handler http.Handler, mount, marker string) {
	t.Helper()
	w := get(handler, mount)
	require.Equal(t, http.StatusMovedPermanently, w.Code, mount)
	require.Equal(t, mount+"/", w.Header().Get("Location"))
	for _, page := range []string{mount + "/", mount + "/index.html", mount + "/customers/42"} {
		w = get(handler, page)
		require.Equal(t, http.StatusOK, w.Code, page)
		require.Contains(t, w.Body.String(), `<base href="`+mount+`/">`, page)
		require.Contains(t, w.Body.String(), marker, page)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"), page)
	}
	w = get(handler, mount+"/config.json")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, json.Valid(w.Body.Bytes()), w.Body.String())
	w = get(handler, mount+"/assets/index-1a2b3c.js")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "console.log('console')", w.Body.String())
	require.Contains(t, w.Header().Get("Cache-Control"), "immutable")
}

// standaloneConsole serves c from a standalone build on a server whose issuer
// is issuer.
func standaloneConsole(issuer string, c *server.AdminConsole) func(*server.Config, *server.Deps) {
	return func(cfg *server.Config, deps *server.Deps) {
		cfg.Auth.Token.Issuer = issuer
		cfg.AdminConsole = c
		deps.Engine.ConsoleAssets = consoleBuild("standalone")
	}
}

// The console's bootstrap document names the path AuthKit's JSON API is
// served at: /auth/v1 beneath an origin issuer, else the issuer's path plus
// /v1, the version AuthKit appends.
func TestAdminConsoleFindsAuthKit(t *testing.T) {
	f := newFixture(t)
	for issuer, want := range map[string]string{
		"http://127.0.0.1":             "/auth/v1",
		"http://127.0.0.1/" + f.schema: "/" + f.schema + "/v1",
	} {
		srv := f.newServer(t, standaloneConsole(issuer, &server.AdminConsole{}))
		handler, err := standaloneHandler(srv)
		require.NoError(t, err)
		requireConsoleAt(t, handler, "/admin", "standalone")

		w := get(handler, "/admin/config.json")
		var boot struct {
			AuthBaseURL string `json:"auth_base_url"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&boot))
		require.Equal(t, want, boot.AuthBaseURL, issuer)

		w = get(handler, boot.AuthBaseURL+"/capabilities")
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", issuer, w.Body.String())
	}
}

// admin_console.path moves the standalone console (#1127): the server's
// handler and its routes on a host router both serve it there and nothing at
// /admin, and a path over OpenRails' own routes refuses to build the server.
// Without a console selected no console route exists.
func TestStandaloneAdminConsolePath(t *testing.T) {
	f := newFixture(t)
	moved := f.newServer(t, standaloneConsole("http://127.0.0.1", &server.AdminConsole{Path: "/billing/admin"}))
	handler, err := standaloneHandler(moved)
	require.NoError(t, err)
	routes, err := moved.Routes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	for _, h := range []http.Handler{handler, mux} {
		requireConsoleAt(t, h, "/billing/admin", "standalone")
		require.Equal(t, http.StatusNotFound, get(h, "/admin/").Code)
	}
	off, err := standaloneHandler(f.newServer(t, standaloneConsole("http://127.0.0.1", nil)))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, get(off, "/admin/").Code, "off unless mounted")

	// A hosted product's console extensions read their data from config.json.
	hosted, err := standaloneHandler(f.newServer(t, standaloneConsole("http://127.0.0.1", &server.AdminConsole{Extensions: map[string]any{"hosted": map[string]any{"plans": []any{"starter"}}}})))
	require.NoError(t, err)
	require.Contains(t, get(hosted, "/admin/config.json").Body.String(), `"extensions":{"hosted":{"plans":["starter"]}}`)
	_, err = f.buildServer(t, standaloneConsole("http://127.0.0.1", &server.AdminConsole{Extensions: map[string]any{"Hosted": true}}))
	require.ErrorContains(t, err, `invalid Routes.AdminConsole.Extensions key "Hosted"`)

	_, err = f.buildServer(t, standaloneConsole("http://127.0.0.1", &server.AdminConsole{Path: "/v1"}))
	require.ErrorContains(t, err, `admin_console.path "/v1" overlaps`)
	_, err = f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1", &server.AdminConsole{})(cfg, deps)
		cfg.RouteGroups = openrails.RouteGroups{Programmatic: true}
	})
	require.ErrorContains(t, err, "turn on at least one of route_groups.admin")
}

// An embedded host mounts the console with a staff route group, from its own
// build, at Routes.Prefix's /admin; the console finds the API there and
// AuthKit at its default /api/v1 on the same origin. A missing or broken
// build, or no staff group, fails the mount; without AdminConsole no console
// route exists.
func TestEmbeddedHostMountsAdminConsole(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: uniqueName("console")}
	staff := openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool}
	deny := authtest.Deny{}
	boot := func(assets fstest.MapFS) *openrails.Client {
		deps := staff
		deps.ConsoleAssets = assets
		client, err := openrails.New(t.Context(), cfg, deps)
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close(context.Background()) })
		return client
	}
	routes := openrails.Routes{Auth: deny, Prefix: "/billing", RouteGroups: adminGroups, AdminConsole: true}
	_, err := boot(fstest.MapFS{}).Routes(routes)
	require.ErrorContains(t, err, "needs a console build")
	_, err = boot(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}).Routes(routes)
	require.ErrorContains(t, err, `<base href="/admin/">`)

	client := boot(consoleBuild("host"))
	_, err = client.Routes(openrails.Routes{Auth: deny, Prefix: "/billing", AdminConsole: true})
	require.ErrorContains(t, err, "turn on at least one of RouteGroups.Admin")

	// The host keeps its own /admin pages.
	root := http.NewServeMux()
	root.HandleFunc("/admin/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("host admin")) })
	require.NoError(t, openrailshttp.Mount(root, client, routes))
	requireConsoleAt(t, root, "/billing/admin", "host")
	require.Equal(t, "host admin", get(root, "/admin/").Body.String())
	w := get(root, "/billing/admin/config.json")
	var bootstrap struct {
		AuthBaseURL string `json:"auth_base_url"`
		APIBaseURL  string `json:"api_base_url"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&bootstrap))
	require.Equal(t, "/api/v1", bootstrap.AuthBaseURL)
	require.Equal(t, "/billing/v1", bootstrap.APIBaseURL)

	off := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(off, client, openrails.Routes{Auth: deny, Prefix: "/billing", RouteGroups: adminGroups}))
	require.Equal(t, http.StatusNotFound, get(off, "/billing/admin/").Code, "not selected, not mounted")
}

// An embedded console signs staff in at the host's AuthKit wherever the host
// mounts it: config.json names Routes.AdminConsoleAuthBase, AuthKit's
// APIBase(), and AuthKit answers there; left empty, AuthKit's default
// /api/v1. A base that is not a path on the console's origin fails the mount.
func TestEmbeddedConsoleFindsHostAuthKit(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: uniqueName("console")}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, ConsoleAssets: consoleBuild("host")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	routes := openrails.Routes{Auth: authtest.Deny{}, Prefix: "/billing", RouteGroups: adminGroups, AdminConsole: true}

	for _, c := range []struct{ apiPath, base, want string }{
		{"", "", "/api/v1"},
		{"/auth", "/auth/v1", "/auth/v1"},
	} {
		ak, err := authkit.New(t.Context(), authkit.Config{
			Token:        authkit.TokenConfig{Issuer: "http://127.0.0.1", IssuedAudiences: []string{"e2e"}},
			Keys:         authkit.KeysConfig{Path: t.TempDir(), AllowEphemeralDevKeys: true},
			Registration: authkit.RegistrationConfig{AllowMissingSenders: true},
			HTTP:         &authkit.HTTPConfig{APIPath: c.apiPath, DirectPeerIP: true},
			Database:     authkit.DatabaseConfig{Schema: f.authSchema()},
		}, authkit.Deps{Postgres: f.pool})
		require.NoError(t, err)
		t.Cleanup(func() { _ = ak.Close(context.Background()) })
		require.Equal(t, c.want, ak.APIBase())

		host := http.NewServeMux()
		require.NoError(t, ak.Mount(host))
		mount := routes
		mount.AdminConsoleAuthBase = c.base
		require.NoError(t, openrailshttp.Mount(host, client, mount))
		w := get(host, "/billing/admin/config.json")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var boot struct {
			AuthBaseURL string `json:"auth_base_url"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&boot))
		require.Equal(t, c.want, boot.AuthBaseURL)
		w = get(host, boot.AuthBaseURL+"/capabilities")
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", boot.AuthBaseURL, w.Body.String())
		if c.base != "" {
			require.Equal(t, http.StatusNotFound, get(host, "/api/v1/capabilities").Code, "nothing at AuthKit's default")
		}
	}

	for _, bad := range []string{"api/v1", "/api/v1/", "/", "https://billing.example.com/api/v1", "//billing.example.com/api/v1", "/api/../v1", "/api v1"} {
		mount := routes
		mount.AdminConsoleAuthBase = bad
		err := openrailshttp.Mount(http.NewServeMux(), client, mount)
		require.ErrorContains(t, err, fmt.Sprintf("invalid Routes.AdminConsoleAuthBase %q", bad))
	}
}

// The embedded console's Roles page asks the host's AuthKit who holds what in
// the two scopes staff hold OpenRails' permissions in: the root group, whose
// holders act at every merchant, and the merchant's own, which a host that
// grants only root roles never creates. With no group for the merchant the
// page's calls still answer: nobody there, and the root group's roles with
// their holders. With the merchant's group, its holders too.
func TestEmbeddedConsoleRolesShowRootAndMerchantStaff(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, uniqueName("roles"))
	mid := client.MerchantID().String()

	rbac := authkit.NewRoles(authkit.CustomRoles)
	merchant := rbac.Persona(billing.MerchantGroupPersona, authkit.CustomRoles)
	perms := map[billing.Permission]iam.Perm{}
	for _, p := range billing.Permissions() {
		if p.Persona() == billing.MerchantGroupPersona {
			perms[p] = merchant.Permission(p.Resource(), p.Action())
		} else {
			perms[p] = rbac.Root.Permission(p.Resource(), p.Action())
		}
	}
	support := rbac.Root.Role("support", perms[billing.RootBillingRead], perms[billing.RootBillingManage])
	admin := rbac.Root.Role("staff-admin", perms[billing.RootBillingRead], rbac.Root.Members.Read, rbac.Root.Roles.Read)
	manager := merchant.Role("manager", perms[billing.MerchantBillingRead], merchant.Members.Read, merchant.Roles.Read)
	ak, _ := authkittest.New(t,
		authkittest.WithDeps(func(d *authkit.Deps) { d.Postgres = f.pool }),
		authkittest.WithConfig(func(c *authkit.Config) { c.Roles = rbac }))

	host := http.NewServeMux()
	require.NoError(t, ak.Mount(host))
	require.NoError(t, openrailshttp.Mount(host, client, openrails.Routes{Auth: ak.Authenticator(), Prefix: "/billing", RouteGroups: adminGroups}))

	person := func(group iam.GroupRef, role iam.Role) (string, string) {
		u := authkittest.NewUser(t, ak)
		authkittest.GrantRole(t, ak, group, iam.UserSubject(u.ID), role)
		return u.ID, authkittest.SignIn(t, ak, u).AccessToken
	}
	type permissionSet struct {
		Role        *string  `json:"role"`
		Permissions []string `json:"permissions"`
	}
	// The console's calls: the access read, then AuthKit's.
	scopeOf := func(token string) string {
		w := call(t, host, token, http.MethodGet, "/billing/v1/admin/access", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return decode[billing.AdminAccess](t, w).ScopeID
	}
	me := func(token, group string) permissionSet {
		w := call(t, host, token, http.MethodGet, ak.APIBase()+"/me/permissions?group_id="+group, "", nil)
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", group, w.Body.String())
		return decode[permissionSet](t, w)
	}
	// holders are a group's members by role, as its roles list them.
	holders := func(token, group string) map[string][]string {
		w := call(t, host, token, http.MethodGet, ak.APIBase()+"/groups/"+group+"/roles", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		out := map[string][]string{}
		for _, role := range decode[struct{ Data []iam.GroupRole }](t, w).Data {
			out[role.Name.String()] = []string{}
		}
		w = call(t, host, token, http.MethodGet, ak.APIBase()+"/groups/"+group+"/members?kind=user&expand=user", "", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		for _, member := range decode[struct{ Data []iam.GroupMember }](t, w).Data {
			out[member.Role.String()] = append(out[member.Role.String()], member.Subject.ID)
		}
		return out
	}

	// Root roles only: the merchant has no group.
	first, _ := person(iam.RootGroup(), support)
	second, supportToken := person(iam.RootGroup(), support)
	adminID, adminToken := person(iam.RootGroup(), admin)
	require.Equal(t, mid, scopeOf(adminToken))
	require.Equal(t, permissionSet{Permissions: []string{}}, me(adminToken, mid), "no group: nobody, not an error")
	inRoot := me(adminToken, "root")
	require.Equal(t, admin.String(), *inRoot.Role)
	require.Subset(t, inRoot.Permissions, []string{string(billing.RootBillingRead), rbac.Root.Roles.Read.String(), rbac.Root.Members.Read.String()})
	staff := holders(adminToken, "root")
	require.ElementsMatch(t, []string{first, second}, staff[support.String()], "the holders of root:billing:read and root:billing:manage")
	require.Equal(t, []string{adminID}, staff[admin.String()])
	// Staff holding only OpenRails' root: names act, and AuthKit shows them no one.
	require.Equal(t, mid, scopeOf(supportToken))
	require.ElementsMatch(t, []string{string(billing.RootBillingRead), string(billing.RootBillingManage)}, me(supportToken, "root").Permissions)
	require.Equal(t, http.StatusForbidden, call(t, host, supportToken, http.MethodGet, ak.APIBase()+"/groups/root/members", "", nil).Code)

	// The merchant's own group: its holders, and nobody on root.
	_, err := ak.CreateGroup(t.Context(), iam.NewGroup{ID: mid, Persona: merchant.Persona})
	require.NoError(t, err)
	managerID, managerToken := person(iam.GroupByID(mid), manager)
	require.Equal(t, mid, scopeOf(managerToken))
	require.Equal(t, permissionSet{Permissions: []string{}}, me(managerToken, "root"))
	require.Equal(t, manager.String(), *me(managerToken, mid).Role)
	require.Equal(t, []string{managerID}, holders(managerToken, mid)[manager.String()])
	require.Equal(t, http.StatusForbidden, call(t, host, managerToken, http.MethodGet, ak.APIBase()+"/groups/root/members", "", nil).Code)

	// Both: someone holding a role in each reads each.
	authkittest.GrantRole(t, ak, iam.GroupByID(mid), iam.UserSubject(adminID), manager)
	require.Equal(t, admin.String(), *me(adminToken, "root").Role)
	require.Equal(t, manager.String(), *me(adminToken, mid).Role)
	require.ElementsMatch(t, []string{first, second}, holders(adminToken, "root")[support.String()])
	require.ElementsMatch(t, []string{managerID, adminID}, holders(adminToken, mid)[manager.String()])
}
