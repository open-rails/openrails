//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/stretchr/testify/require"
)

// gateRefusals are the codes the route gate refuses with before a handler
// runs; any other answer is the admitted request's own.
var gateRefusals = []string{
	"authentication_required", "permission_required", "application_required", "merchant_unresolved",
	"invoker_scoped_principal", "authentication_unavailable", "authorization_unavailable",
	"credential_expired", "credential_revoked",
}

var routeWildcard = regexp.MustCompile(`\{[^}]+\}`)

// probeRoute sends route's request with credential, each path wildcard a
// UUID and a write's body an empty object, and answers its status and code.
// A SCIM refusal carries no code: SCIM's error body has none.
func probeRoute(t *testing.T, a *authorityWorld, route httproutes.Route, credential string) (int, string) {
	t.Helper()
	var body any
	if route.Method != http.MethodGet && route.Method != http.MethodDelete {
		body = map[string]any{}
	}
	status, raw := a.request(t, route.Method, routeWildcard.ReplaceAllString(route.Path, "11111111-1111-4111-8111-111111111111"), credential, body)
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	return status, envelope.Error.Code
}

// gateRefused reports an answer of the route gate, not of the handler.
func gateRefused(status int, code string) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden && code == "" || slices.Contains(gateRefusals, code)
}

// roleKey is a new API key of alpha's, an application holding exactly perms
// there through a custom role named name.
func roleKey(t *testing.T, a *authorityWorld, name string, perms ...billing.Permission) string {
	t.Helper()
	granted := make([]iam.Perm, 0, len(perms))
	for _, p := range perms {
		granted = append(granted, mustPermission(t, string(p)))
	}
	role, err := a.auth.CreateGroupRole(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewGroupRole{Name: name, Permissions: granted})
	require.NoError(t, err)
	key, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: name, Role: role.Name})
	require.NoError(t, err)
	return key.Secret
}

// merchantPermissions are billing.Permissions' merchant: names but those
// left out.
func merchantPermissions(out ...billing.Permission) []billing.Permission {
	var kept []billing.Permission
	for _, p := range billing.Permissions() {
		if p.Persona() == billing.MerchantGroupPersona && !slices.Contains(out, p) {
			kept = append(kept, p)
		}
	}
	return kept
}

func roleName(prefix string, p billing.Permission) string {
	return prefix + "-" + p.Resource() + "-" + p.Action()
}

func TestRoutePermissions(t *testing.T) {
	t.Run("OR-J01-S20/every-route-refuses-a-credential-without-its-permission", func(t *testing.T) {
		// Each root: name's twin role holds it alone in the root scope, and
		// root:admin-rate-limits:unlock, which only the hold clear asks for.
		twins := map[billing.Permission]iam.Role{}
		a := newAuthorityWorldWithAuth(t, func(c *authkit.Config) {
			for _, p := range billing.Permissions() {
				if p.Persona() == "root" && p != billing.RootAdminRateLimitsUnlock {
					twins[p] = c.Roles.Root.Role(roleName("twin", p), mustPermission(t, string(p)), mustPermission(t, string(billing.RootAdminRateLimitsUnlock)))
				}
			}
		}, func(w *world) { w.deps.UserInfo = nil })
		groups := openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}
		mountedRoutes, err := a.engine.Routes(openrails.Routes{Auth: a.auth.Authenticator(), RouteGroups: groups})
		require.NoError(t, err)
		mounted := map[string]bool{}
		for _, route := range mountedRoutes {
			mounted[route.Method+" "+route.Path] = true
		}
		// lacking holds every merchant: permission but one; holding that one
		// alone; twin is a person holding a root: name and nothing at alpha.
		lacking, holding, twin := map[billing.Permission]string{}, map[billing.Permission]string{}, map[billing.Permission]string{}
		credential := func(cache map[billing.Permission]string, p billing.Permission, mint func() string) string {
			if _, ok := cache[p]; !ok {
				cache[p] = mint()
			}
			return cache[p]
		}
		checked := 0
		for _, route := range httproutes.Catalog() {
			pair := route.Needs()
			switch route.Auth {
			case httproutes.AuthMerchant, httproutes.AuthApplication, httproutes.AuthProvisioning:
				require.Len(t, pair.Names(), 2, "%s names its merchant: and root: permission", route.Key())
			default:
				require.Empty(t, pair.Names(), "%s's tier checks no permission", route.Key())
				continue
			}
			if !mounted[route.Key()] {
				require.NotEqual(t, httproutes.Always, route.When, "%s is mounted in every deployment", route.Key())
				continue
			}
			checked++
			key := credential(lacking, pair.Merchant, func() string {
				return roleKey(t, a, roleName("lacks", pair.Merchant), merchantPermissions(pair.Merchant)...)
			})
			status, code := probeRoute(t, a, route, key)
			require.Equal(t, http.StatusForbidden, status, "%s admitted a credential without %s (%s)", route.Key(), pair.Merchant, code)
			if code != "" {
				require.Equal(t, string(billing.CodePermissionRequired), code, route.Key())
			}
			person := credential(twin, pair.Root, func() string {
				user := authtest.NewUser(t, a.auth)
				authtest.GrantRole(t, a.auth, iam.RootGroup(), iam.UserSubject(user.ID), twins[pair.Root])
				return authtest.SignIn(t, a.auth, user).AccessToken
			})
			status, code = probeRoute(t, a, route, person)
			if route.Auth == httproutes.AuthMerchant {
				require.False(t, gateRefused(status, code), "%s refused its root twin %s: %d %s", route.Key(), pair.Root, status, code)
				continue
			}
			// A programmatic route takes an application: a person is refused
			// whatever it holds. An AuthKit application is bound to its own
			// group, so its merchant: name is what admits it.
			require.Equal(t, http.StatusForbidden, status, route.Key())
			if code != "" {
				require.Equal(t, string(billing.CodeApplicationRequired), code, route.Key())
			}
			key = credential(holding, pair.Merchant, func() string {
				return roleKey(t, a, roleName("holds", pair.Merchant), pair.Merchant)
			})
			status, code = probeRoute(t, a, route, key)
			require.False(t, gateRefused(status, code), "%s refused an application holding %s: %d %s", route.Key(), pair.Merchant, status, code)
		}
		require.Greater(t, checked, 100, "the catalog's gated routes are mounted and probed")
		t.Logf("probed %d mounted routes of the catalog", checked)
	})
	t.Run("OR-J01-S21/a-config-only-credential-cannot-approve-a-refund", func(t *testing.T) {
		a := newAuthorityWorld(t)
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		raise := func(subject string, rec recommend.Recommendation) billing.FindingID {
			t.Helper()
			var finding reconcile.FindingRecord
			require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
				var err error
				finding, err = (&reconcile.PGStore{DB: database, Now: a.clock.Now}).RaiseFinding(ctx, reconcile.RaisedFinding{Type: "consistency.duplicate.ownership",
					SubjectKey: subject, Severity: reconcile.SeverityHigh, Evidence: map[string]any{recommend.EvidenceKey: rec.Map()}})
				return err
			}))
			return billing.FindingID(finding.ID)
		}
		refund := raise("ownership:refund", recommend.CancelAndRefundRec(billing.SubscriptionID{}, billing.PaymentID(uuid.New())))
		ack := raise("ownership:acknowledge", recommend.Recommendation{Action: recommend.ActionAckResume})
		before, err := a.alpha.GetFinding(t.Context(), refund)
		require.NoError(t, err)

		config := a.remote(t, a.alpha.MerchantID(), roleKey(t, a, "config-only", billing.MerchantConfigRead, billing.MerchantConfigManage))
		_, err = config.ResolveFinding(t.Context(), refund, billing.ResolveFindingParams{Outcome: billing.FindingApprove, Notes: "refund the later purchase"})
		requireCode(t, err, billing.CodePermissionRequired)
		after, err := a.alpha.GetFinding(t.Context(), refund)
		require.NoError(t, err)
		require.Equal(t, before.Status, after.Status, "a refused approval leaves the finding as it was")
		// A resolution that touches no customer stays the configuration's.
		_, err = config.ResolveFinding(t.Context(), ack, billing.ResolveFindingParams{Outcome: billing.FindingApprove, Notes: "acknowledged"})
		require.NoError(t, err)

		// Holding customer support's manage permission too, the approval runs
		// its action, which finds no such payment.
		both := a.remote(t, a.alpha.MerchantID(), roleKey(t, a, "config-and-support", billing.MerchantConfigManage, billing.MerchantCustomerManage))
		_, err = both.ResolveFinding(t.Context(), refund, billing.ResolveFindingParams{Outcome: billing.FindingApprove, Notes: "refund the later purchase"})
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.NotEqual(t, billing.CodePermissionRequired, refusal.Code, refusal.Error())
	})
}

const scimUsers = "/v1/app/scim/v2/Users"

func scimUser(id, name string) map[string]any {
	return map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": name, "displayName": name}
}

func scimBulk(operations ...map[string]any) map[string]any {
	return map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:BulkRequest"}, "Operations": operations}
}

func TestDirectoryAuthority(t *testing.T) {
	t.Run("OR-J07-S07/an-application-without-the-provisioning-permission-is-refused", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) { w.deps.UserInfo = nil })
		without := roleKey(t, a, "no-provisioning", merchantPermissions(billing.MerchantProvisioningRead, billing.MerchantProvisioningManage)...)
		reader := roleKey(t, a, "provisioning-reader", billing.MerchantProvisioningRead)
		writer := roleKey(t, a, "provisioning-writer", billing.MerchantProvisioningManage)
		refused := uuid.NewString()
		for _, credential := range []string{without, reader, a.aliceToken} {
			for _, request := range []struct {
				method, path string
				body         any
			}{
				{"POST", scimUsers, scimUser(refused, "refused")},
				{"PUT", scimUsers + "/" + refused, scimUser(refused, "refused")},
				{"DELETE", scimUsers + "/" + refused, nil},
				{"POST", "/v1/app/scim/v2/Bulk", scimBulk(map[string]any{"method": "POST", "path": "/Users", "bulkId": "b", "data": scimUser(refused, "refused")})},
			} {
				status, raw := a.request(t, request.method, request.path, credential, request.body)
				require.Equal(t, http.StatusForbidden, status, request.method+" "+request.path+": "+string(raw))
			}
		}
		for _, credential := range []string{without, writer, a.aliceToken} {
			status, raw := a.request(t, "GET", scimUsers, credential, nil)
			require.Equal(t, http.StatusForbidden, status, string(raw))
		}
		status, raw := a.request(t, "GET", scimUsers+"/"+refused, reader, nil)
		require.Equal(t, http.StatusNotFound, status, "a refused write provisions no User: "+string(raw))

		admitted := uuid.NewString()
		status, raw = a.request(t, "POST", scimUsers, writer, scimUser(admitted, "admitted"))
		require.Equal(t, http.StatusCreated, status, string(raw))
		status, raw = a.request(t, "GET", scimUsers+"/"+admitted, reader, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), "admitted")
		// The provisioning token is the explicit exception: it is its own grant.
		token, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		status, raw = a.request(t, "GET", scimUsers+"/"+admitted, token.Token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		customer, err := a.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(admitted)))
		require.NoError(t, err)
		require.NotNil(t, customer.Contact)
		require.Equal(t, "admitted", *customer.Contact.Name)
	})
	t.Run("OR-J07-S08/erasures-spend-the-destructive-budget-and-name-their-actor", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			w.cfg.DestructiveActionLimits = openrails.DestructiveActionLimitsConfig{ActorPerHour: 2}
		})
		token, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		users := make([]string, 4)
		for i := range users {
			users[i] = uuid.NewString()
			status, raw := a.request(t, "POST", scimUsers, token.Token, scimUser(users[i], "user-"+users[i][:8]))
			require.Equal(t, http.StatusCreated, status, string(raw))
		}
		erasure := func(target string) (kind, issuer, actor, state string) {
			t.Helper()
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT kind, actor_issuer, actor_id, state FROM "+pgx.Identifier{a.schema, "destructive_actions"}.Sanitize()+" WHERE merchant_id = $1 AND target = $2",
				a.alpha.MerchantID().UUID(), target).Scan(&kind, &issuer, &actor, &state))
			return kind, issuer, actor, state
		}
		present := func(credential, id string, want int) {
			t.Helper()
			status, raw := a.request(t, "GET", scimUsers+"/"+id, credential, nil)
			require.Equal(t, want, status, string(raw))
		}

		// One erasure, admitted and attributed to the token.
		status, raw := a.request(t, "DELETE", scimUsers+"/"+users[0], token.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		present(token.Token, users[0], http.StatusNotFound)
		kind, issuer, actor, state := erasure(users[0])
		require.Equal(t, "erase_contact", kind)
		require.Equal(t, httproutes.ProvisioningIssuer, issuer)
		require.Equal(t, token.ID.String(), actor)
		require.Equal(t, "succeeded", state)

		// A Bulk whose erasures exceed the actor's budget erases nothing and
		// opens a hold; the hold then refuses the actor's next erasure.
		bulk := scimBulk(
			map[string]any{"method": "PATCH", "path": "/Users/" + users[1], "data": map[string]any{"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": []any{map[string]any{"op": "replace", "path": "displayName", "value": "Must Not Apply"}}}},
			map[string]any{"method": "DELETE", "path": "/Users/" + users[1]},
			map[string]any{"method": "DELETE", "path": "/Users/" + users[2]},
		)
		status, raw = a.request(t, "POST", "/v1/app/scim/v2/Bulk", token.Token, bulk)
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		for _, id := range users[1:] {
			present(token.Token, id, http.StatusOK)
		}
		status, raw = a.request(t, "GET", scimUsers+"/"+users[1], token.Token, nil)
		require.Equal(t, http.StatusOK, status)
		require.NotContains(t, string(raw), "Must Not Apply")
		holds, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
		require.NoError(t, err)
		require.Len(t, holds.Items, 1)
		hold := holds.Items[0]
		require.Equal(t, "actor", hold.Scope)
		require.Equal(t, "erase_contact", hold.Action)
		require.EqualValues(t, 1, hold.Observed)
		require.EqualValues(t, 2, hold.Requested)
		require.EqualValues(t, 2, hold.Ceiling)
		trigger, target, _ := holdEvidence(t, a, hold)
		require.Equal(t, httproutes.ProvisioningIssuer, trigger.Issuer)
		require.Equal(t, token.ID.String(), trigger.ID)
		require.Equal(t, users[1], target)
		status, raw = a.request(t, "DELETE", scimUsers+"/"+users[3], token.Token, nil)
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		present(token.Token, users[3], http.StatusOK)

		// Another actor's budget is its own, and its erasure names it.
		directory := roleKey(t, a, "directory", billing.MerchantProvisioningRead, billing.MerchantProvisioningManage)
		status, raw = a.request(t, "DELETE", scimUsers+"/"+users[3], directory, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		present(token.Token, users[3], http.StatusNotFound)
		kind, issuer, actor, state = erasure(users[3])
		require.Equal(t, "erase_contact", kind)
		require.NotEqual(t, httproutes.ProvisioningIssuer, issuer)
		require.NotEmpty(t, strings.TrimSpace(issuer))
		require.NotEmpty(t, actor)
		require.Equal(t, "succeeded", state)
		_, err = a.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(users[3])))
		require.NoError(t, err, "an erasure keeps the customer's billing")
	})
}
