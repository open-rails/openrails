//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/failpoint"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/proposals"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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

// catalogProbe is a world's credentials for probing the route catalog: per
// merchant: name a key holding every other one and a key holding it alone;
// per root: name a person holding it alone (with root:admin-rate-limits:unlock,
// which only the hold clear asks for) and a person holding every other root:
// name.
type catalogProbe struct {
	*authorityWorld
	twins, others                     map[billing.Permission]iam.Role
	lacking, holding, twin, otherRoot map[billing.Permission]string
}

func newCatalogProbe(t *testing.T, configure ...func(*world)) *catalogProbe {
	t.Helper()
	p := &catalogProbe{twins: map[billing.Permission]iam.Role{}, others: map[billing.Permission]iam.Role{},
		lacking: map[billing.Permission]string{}, holding: map[billing.Permission]string{}, twin: map[billing.Permission]string{}, otherRoot: map[billing.Permission]string{}}
	var roots []billing.Permission
	for _, perm := range billing.Permissions() {
		if perm.Persona() == "root" {
			roots = append(roots, perm)
		}
	}
	p.authorityWorld = newAuthorityWorldWithAuth(t, func(c *authkit.Config) {
		for _, root := range roots {
			if root == billing.RootAdminRateLimitsUnlock {
				continue
			}
			p.twins[root] = c.Roles.Root.Role(roleName("twin", root), mustPermission(t, string(root)), mustPermission(t, string(billing.RootAdminRateLimitsUnlock)))
			var rest []iam.Grant
			for _, other := range roots {
				if other != root {
					rest = append(rest, mustPermission(t, string(other)))
				}
			}
			p.others[root] = c.Roles.Root.Role(roleName("others", root), rest...)
		}
	}, append([]func(*world){func(w *world) { w.deps.UserInfo = nil }}, configure...)...)
	return p
}

// credential is cache's credential for perm, minted once.
func credential(cache map[billing.Permission]string, perm billing.Permission, mint func() string) string {
	if _, ok := cache[perm]; !ok {
		cache[perm] = mint()
	}
	return cache[perm]
}

// rootPerson signs in a new person holding role in the root scope only.
func (p *catalogProbe) rootPerson(t *testing.T, role iam.Role) string {
	t.Helper()
	user := authtest.NewUser(t, p.auth)
	authtest.GrantRole(t, p.auth, iam.RootGroup(), iam.UserSubject(user.ID), role)
	return authtest.SignIn(t, p.auth, user).AccessToken
}

// probe sends every mounted catalog route on a tier that checks a permission:
// a key with every merchant: name but its own, and a person with every root:
// name but its own, are refused; a person holding its root: name alone is
// admitted on a merchant route and refused application_required on a
// programmatic one, where a key holding its merchant: name alone is admitted.
// It answers how many it probed and the keys the world leaves unmounted.
func (p *catalogProbe) probe(t *testing.T) (int, []string) {
	t.Helper()
	groups := openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}
	mountedRoutes, err := p.engine.Routes(openrails.Routes{Auth: p.auth.Authenticator(), RouteGroups: groups})
	require.NoError(t, err)
	mounted := map[string]bool{}
	for _, route := range mountedRoutes {
		mounted[route.Method+" "+route.Path] = true
	}
	checked, unmounted := 0, []string{}
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
			unmounted = append(unmounted, route.Key())
			continue
		}
		checked++
		refused := func(credential string, code billing.ErrorCode, who string) {
			t.Helper()
			status, got := probeRoute(t, p.authorityWorld, route, credential)
			require.Equal(t, http.StatusForbidden, status, "%s admitted %s (%s)", route.Key(), who, got)
			if got != "" {
				require.Equal(t, string(code), got, route.Key())
			}
		}
		refused(credential(p.lacking, pair.Merchant, func() string {
			return roleKey(t, p.authorityWorld, roleName("lacks", pair.Merchant), merchantPermissions(pair.Merchant)...)
		}), billing.CodePermissionRequired, "a key without "+string(pair.Merchant))
		person := credential(p.twin, pair.Root, func() string { return p.rootPerson(t, p.twins[pair.Root]) })
		others := credential(p.otherRoot, pair.Root, func() string { return p.rootPerson(t, p.others[pair.Root]) })
		if route.Auth == httproutes.AuthMerchant {
			refused(others, billing.CodePermissionRequired, "a person with every root: name but "+string(pair.Root))
			status, code := probeRoute(t, p.authorityWorld, route, person)
			require.False(t, gateRefused(status, code), "%s refused its root twin %s: %d %s", route.Key(), pair.Root, status, code)
			continue
		}
		// A programmatic route takes an application: a person is refused
		// whatever it holds. An AuthKit application is bound to its own
		// group, so its merchant: name is what admits it.
		refused(person, billing.CodeApplicationRequired, "a person holding "+string(pair.Root))
		refused(others, billing.CodeApplicationRequired, "a person with every other root: name")
		key := credential(p.holding, pair.Merchant, func() string {
			return roleKey(t, p.authorityWorld, roleName("holds", pair.Merchant), pair.Merchant)
		})
		status, code := probeRoute(t, p.authorityWorld, route, key)
		require.False(t, gateRefused(status, code), "%s refused an application holding %s: %d %s", route.Key(), pair.Merchant, status, code)
	}
	return checked, unmounted
}

func TestRoutePermissions(t *testing.T) {
	t.Run("OR-J01-S20/every-route-refuses-a-credential-without-its-permission", func(t *testing.T) {
		// A file-configured merchant mounts every gated route but these; a new
		// one this world leaves out is named here.
		checked, unmounted := newCatalogProbe(t).probe(t)
		require.ElementsMatch(t, []string{
			"PUT /v1/merchant/custodians/{key}",
			"POST /v1/merchant/psps",
			"PATCH /v1/merchant/psps/{id}",
			"PATCH /v1/merchant/configuration",
			"POST /v1/merchant/alert-webhooks",
			"DELETE /v1/merchant/alert-webhooks/{id}",
			"PATCH /v1/merchant/alert-webhooks/{id}",
		}, unmounted)
		require.Greater(t, checked, 100, "the catalog's gated routes are mounted and probed")

		// With Vault-held configuration every gated route mounts: the
		// configuration edits are probed too.
		addr, token := os.Getenv("OPENRAILS_CRITICAL_VAULT_ADDR"), os.Getenv("OPENRAILS_CRITICAL_VAULT_TOKEN")
		require.NotEmpty(t, addr, "the configuration edits need real Vault")
		require.NotEmpty(t, token)
		t.Setenv(vaulttest.AddrEnv, addr)
		t.Setenv(vaulttest.TokenEnv, token)
		vault := vaulttest.New(t)
		all, none := newCatalogProbe(t, func(w *world) {
			w.cfg.Vault = vault.Config()
			w.deps.Vault = vault.Client()
		}).probe(t)
		require.Empty(t, none, "every gated route mounts with Vault")
		require.Equal(t, checked+len(unmounted), all)
		t.Logf("probed %d and %d mounted routes of the catalog", checked, all)
	})
	t.Run("OR-J01-S21/a-config-only-credential-cannot-approve-a-refund", func(t *testing.T) {
		a := newAuthorityWorld(t)
		refund := raiseRecommendation(t, a, "ownership:refund", recommend.CancelAndRefundRec(billing.SubscriptionID{}, billing.PaymentID(uuid.New())))
		ack := raiseRecommendation(t, a, "ownership:acknowledge", recommend.Recommendation{Action: recommend.ActionAckResume})
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
	t.Run("OR-J01-S23/a-finding-approval-spends-its-actions-operation-limit", func(t *testing.T) {
		a := newAuthorityWorld(t)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "limited", DisplayName: "Limited"})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		person := consoleStaff(t, a, "support-person", billing.MerchantConfigManage, billing.MerchantCustomerManage)
		// A full batch of grants spends the person's grant limit for the minute.
		hours := 1
		items := make([]billing.CreateProductAccessParams, billing.MaxBatchItems)
		for i := range items {
			items[i] = billing.CreateProductAccessParams{CustomerID: customer, ProductID: product.ID, Hours: &hours}
		}
		status, raw := a.request(t, "POST", "/v1/admin/product-access", person, billing.CreateProductAccessBatchParams{Items: items}, map[string]string{"Idempotency-Key": uuid.NewString()})
		require.NotEqual(t, http.StatusTooManyRequests, status, string(raw))
		// Approving a grant recommendation grants a product too, so it spends
		// the same limit and is refused.
		grant := raiseRecommendation(t, a, "ownership:grant", recommend.GrantProductRec(customer, product.ID, "legitimize"))
		status, raw = a.request(t, "POST", "/v1/merchant/findings/"+grant.String()+"/resolve", person, billing.ResolveFindingParams{Outcome: billing.FindingApprove, Notes: "legitimize"})
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.Contains(t, string(raw), `"code":"rate_limit_exceeded"`)
		open, err := a.alpha.GetFinding(t.Context(), grant)
		require.NoError(t, err)
		require.Nil(t, open.Resolution, "a refused approval leaves the finding open")
	})
	t.Run("OR-J01-S24/a-psp-import-applies-only-the-corrections-its-approver-may-approve", func(t *testing.T) {
		// A host book holds alice on a legacy price NMI no longer bills: the
		// pull proposes the correction to the provider's price and terms.
		iw := newImportWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "legacy23", Name: "Legacy", Amount: "23.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "current19", Name: "Current", Amount: "19.00", Days: 30})
		prices := iw.premium(t, "legacy23", "current19")
		schedule := iw.nmiMember(t, iw.alice.Email, "current19", "19.00", start)
		paid := iw.clock.Now().Add(5 * 24 * time.Hour)
		res := iw.hostBook(t, start.Add(24*time.Hour), billing.DeclaredSubscription{SourceID: "legacy-alice", Customer: customerOf(iw.alice), Price: prices["legacy23"].ID,
			Rail: "nmi", RailSubscriptionID: schedule, StartedAt: start.Add(5 * 24 * time.Hour), PaidThrough: &paid})
		require.Equal(t, []string{"legacy-alice"}, res.Imported, "%v", res.Reasons)
		imp := iw.manual(t, "nmi")
		correct := proposalOf(t, iw.proposals(t, imp.ID), billing.ChangeCorrectSubscription, subscriptionOf(t, iw, customerOf(iw.alice), schedule).ID.String())

		// A configuration-only person cannot apply it: the correction would
		// move what alice is billed.
		apply := "/v1/merchant/psp-imports/" + imp.ID.String() + "/apply"
		status, raw := iw.request(t, "POST", apply, consoleStaff(t, iw.authorityWorld, "import-person", billing.MerchantConfigManage), nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"permission_required"`)
		held, err := iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.Nil(t, held.Resolution, "a refused apply approves nothing")
		// Holding customer support's manage permission too, the apply runs
		// and approves the correction.
		status, raw = iw.request(t, "POST", apply, consoleStaff(t, iw.authorityWorld, "import-support-person", billing.MerchantConfigManage, billing.MerchantCustomerManage), nil)
		require.Equal(t, http.StatusAccepted, status, string(raw))
		iw.await(t, imp.ID, billing.PSPImportApplied)
		applied, err := iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.NotNil(t, applied.Resolution, "the apply left the correction open; notes %q, evidence %v", deref(applied.Notes), applied.Evidence)
		require.Equal(t, "approved", *applied.Resolution)
	})
	t.Run("OR-J01-S25/an-approval-is-one-operation-however-many-changes-it-carries", func(t *testing.T) {
		a := newAuthorityWorld(t)
		approve := func(person string, ids ...billing.FindingID) (int, []byte) {
			return a.request(t, "POST", "/v1/merchant/proposals/approve", person, billing.ApproveProposalsParams{IDs: ids, Notes: "the provider's terms"},
				map[string]string{"Idempotency-Key": uuid.NewString()})
		}
		corrections := func(n int) []billing.FindingID {
			var ids []billing.FindingID
			for range n {
				ids = append(ids, raiseProposal(t, a, billing.ChangeCorrectSubscription))
			}
			return ids
		}
		// The subscription-change route allows ten operations a minute. One
		// approval of eleven corrections is one operation, and goes through.
		bulk := consoleStaff(t, a, "bulk-person", billing.MerchantConfigManage, billing.MerchantCustomerManage)
		status, raw := approve(bulk, corrections(11)...)
		require.Equal(t, http.StatusOK, status, string(raw))
		// Eleven separate approvals in a minute are eleven: the eleventh is
		// refused.
		single := consoleStaff(t, a, "single-person", billing.MerchantConfigManage, billing.MerchantCustomerManage)
		for i, id := range corrections(11) {
			status, raw := approve(single, id)
			if i < 10 {
				require.Equal(t, http.StatusOK, status, "approval %d: %s", i+1, raw)
				continue
			}
			require.Equal(t, http.StatusTooManyRequests, status, string(raw))
			require.Contains(t, string(raw), `"code":"rate_limit_exceeded"`)
			f, err := a.alpha.GetFinding(t.Context(), id)
			require.NoError(t, err)
			require.Nil(t, f.Resolution, "a refused approval approves nothing")
		}
	})
	t.Run("OR-J01-S26/a-proposal-rewritten-before-its-lock-is-held-to-the-kinds-allowed", func(t *testing.T) {
		a := newAuthorityWorld(t)
		proposal := raiseProposal(t, a, billing.ChangeRecordCharge)
		// A pull rewrites the proposal into a cancel after the approval
		// checked its kind, before the approval locks it.
		cancel, err := json.Marshal(reconcile.Change{FindingChange: billing.FindingChange{Kind: billing.ChangeCancelSubscription}})
		require.NoError(t, err)
		var once sync.Once
		remove := failpoint.Set(func(_ context.Context, site failpoint.Site) error {
			if site.Point != failpoint.BeforeLock || site.Kind != proposals.FailpointKind || site.Operation != proposal.UUID() {
				return nil
			}
			var err error
			once.Do(func() {
				_, err = a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "reconciliation_findings"}.Sanitize()+" SET change = $1 WHERE merchant_id = $2 AND id = $3", cancel, a.alpha.MerchantID().UUID(), proposal.UUID())
			})
			return err
		})
		defer remove()
		status, raw := a.request(t, "POST", "/v1/merchant/proposals/approve", consoleStaff(t, a, "config-person", billing.MerchantConfigManage),
			billing.ApproveProposalsParams{IDs: []billing.FindingID{proposal}, Notes: "the provider's facts"}, map[string]string{"Idempotency-Key": uuid.NewString()})
		require.Equal(t, http.StatusOK, status, string(raw))
		var out billing.ProposalResults
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Len(t, out.Data, 1)
		require.Equal(t, billing.ProposalFailed, out.Data[0].Outcome)
		require.NotNil(t, out.Data[0].Error)
		require.Equal(t, billing.CodePermissionRequired, out.Data[0].Error.Code, out.Data[0].Error.Message)
		f, err := a.alpha.GetFinding(t.Context(), proposal)
		require.NoError(t, err)
		require.Nil(t, f.Resolution)
	})
	t.Run("OR-J01-S22/a-config-only-person-cannot-approve-a-cancel-proposal", func(t *testing.T) {
		a := newAuthorityWorld(t)
		approve := func(token string, finding billing.FindingID) (int, []byte) {
			return a.request(t, "POST", "/v1/merchant/proposals/approve", token, billing.ApproveProposalsParams{IDs: []billing.FindingID{finding}, Notes: "the provider's facts"},
				map[string]string{"Idempotency-Key": uuid.NewString()})
		}
		configOnly := consoleStaff(t, a, "config-person", billing.MerchantConfigManage)
		// A cancel ends access; a correction moves the price and terms a
		// subscription bills on: each is customer support's too, asked before
		// anything runs.
		for _, kind := range []billing.ChangeKind{billing.ChangeCancelSubscription, billing.ChangeCorrectSubscription} {
			proposal := raiseProposal(t, a, kind)
			before, err := a.alpha.GetFinding(t.Context(), proposal)
			require.NoError(t, err)
			status, raw := approve(configOnly, proposal)
			require.Equal(t, http.StatusForbidden, status, "%s: %s", kind, raw)
			require.Contains(t, string(raw), `"code":"permission_required"`)
			after, err := a.alpha.GetFinding(t.Context(), proposal)
			require.NoError(t, err)
			require.Equal(t, before.Status, after.Status, "a refused approval leaves the %s proposal open", kind)
		}
		// A recorded charge only records what the provider billed: the
		// configuration approves it alone.
		status, raw := approve(configOnly, raiseProposal(t, a, billing.ChangeRecordCharge))
		require.Equal(t, http.StatusOK, status, string(raw))
		// Holding customer support's manage permission too, the cancel's
		// approval runs.
		status, raw = approve(consoleStaff(t, a, "config-support-person", billing.MerchantConfigManage, billing.MerchantCustomerManage), raiseProposal(t, a, billing.ChangeCancelSubscription))
		require.Equal(t, http.StatusOK, status, string(raw))
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
		// A create answers the User it made, as SCIM says, though the writer
		// cannot read it back.
		status, raw = a.request(t, "POST", scimUsers, writer, scimUser(admitted, "admitted"))
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Contains(t, string(raw), admitted)
		status, raw = a.request(t, "GET", scimUsers+"/"+admitted, reader, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), "admitted")
		// A token grants SCIM's routes, so minting, listing or revoking one
		// takes the provisioning pair, not merely the configuration's.
		for _, request := range []struct{ method, path string }{{"POST", "/v1/merchant/provisioning-tokens"}, {"GET", "/v1/merchant/provisioning-tokens"}} {
			status, raw := a.request(t, request.method, request.path, without, billing.CreateProvisioningTokenParams{Name: "escalation"})
			require.Equal(t, http.StatusForbidden, status, string(raw))
			require.Contains(t, string(raw), `"code":"permission_required"`)
		}
		tokens := a.remote(t, a.alpha.MerchantID(), roleKey(t, a, "token-minter", billing.MerchantProvisioningManage))
		token, err := tokens.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		// The provisioning token is the explicit exception: it is its own
		// grant, bound to its merchant, and honors the expected-merchant check.
		status, raw = a.request(t, "GET", scimUsers+"/"+admitted, token.Token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		status, raw = a.request(t, "GET", scimUsers+"/"+admitted, token.Token, nil, map[string]string{"OpenRails-Expected-Merchant": a.beta.MerchantID().String()})
		require.Equal(t, http.StatusPreconditionFailed, status, string(raw))
		// An application's access read names the provisioning permissions it
		// holds.
		access, err := a.remote(t, a.alpha.MerchantID(), writer).GetAdminAccess(t.Context())
		require.NoError(t, err)
		require.Contains(t, access.ApplicationPermissions, billing.MerchantProvisioningManage)
		require.NotContains(t, access.ApplicationPermissions, billing.MerchantProvisioningRead)
		customer, err := a.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(admitted)))
		require.NoError(t, err)
		require.NotNil(t, customer.Contact)
		require.Equal(t, "admitted", *customer.Contact.Name)
	})
	t.Run("OR-J07-S09/a-waiting-erasure-takes-the-contact-at-once-and-a-stale-report-cannot-restore-it", func(t *testing.T) {
		mail := &alertMailbox{}
		a, _ := newPaymentWorld(t, func(w *world) {
			w.deps.UserInfo = nil
			w.deps.Email = mail
			w.cfg.DestructiveActionLimits = billing.DestructiveActionLimitsConfig{ErasurePerHour: 1}
		})
		startFleet(t, a.world)
		token, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		provision := func(name string, meta map[string]any) (string, int, []byte) {
			id := uuid.NewString()
			user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": name, "displayName": name,
				"emails": []any{map[string]any{"value": name + "@directory.invalid", "primary": true}}}
			if meta != nil {
				user["meta"] = meta
			}
			status, raw := a.request(t, "POST", scimUsers, token.Token, user)
			return id, status, raw
		}
		spent, status, raw := provision("spent", nil)
		require.Equal(t, http.StatusCreated, status, string(raw))
		held, status, raw := provision("held", nil)
		require.Equal(t, http.StatusCreated, status, string(raw))
		// The first erasure spends the budget of one; the second waits under
		// the erasure hold, and its contact is gone at once.
		for _, id := range []string{spent, held} {
			status, raw := a.request(t, "DELETE", scimUsers+"/"+id, token.Token, nil)
			require.Equal(t, http.StatusNoContent, status, string(raw))
		}
		customer := billing.CustomerID(uuid.MustParse(held))
		read, err := a.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err)
		require.True(t, read.Contact == nil || read.Contact.Email == nil && read.Contact.Name == nil, "no contact: %+v", read.Contact)
		found, err := a.alpha.ListCustomers(t.Context(), billing.CustomerListParams{Search: "held"})
		require.NoError(t, err)
		require.Empty(t, found.Items)
		paidExternally(t, a, customer, messageSale(t, a), "held-receipt")
		log := settledMessages(t, a.world, customer, 1)
		require.Equal(t, billing.MessageNoContact, log[0].Status, "the message has no recipient")
		require.Empty(t, mail.to("held@directory.invalid"))

		// A report dated before the erasure loses to it; one dated after
		// provisions the User again.
		status, raw = a.request(t, "POST", scimUsers, token.Token, map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": held, "userName": "held",
			"emails": []any{map[string]any{"value": "held@directory.invalid", "primary": true}}, "meta": map[string]any{"lastModified": "2020-01-01T00:00:00Z"}})
		require.Equal(t, http.StatusConflict, status, string(raw))
		status, raw = a.request(t, "GET", scimUsers+"/"+held, token.Token, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		read, err = a.alpha.GetCustomer(t.Context(), customer)
		require.NoError(t, err)
		require.True(t, read.Contact == nil || read.Contact.Email == nil, "a stale report restores nothing: %+v", read.Contact)
		status, raw = a.request(t, "POST", scimUsers, token.Token, map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": held, "userName": "held-again"})
		require.Equal(t, http.StatusCreated, status, string(raw))
	})
	t.Run("OR-J07-S08/erasures-keep-their-own-budget-and-are-never-lost", func(t *testing.T) {
		// A root operator holding the unlock and the configuration's root
		// name, and nothing of the provisioning.
		var operator iam.Role
		a := newAuthorityWorldWithAuth(t, func(c *authkit.Config) {
			operator = c.Roles.Root.Role("hold-operator", mustPermission(t, string(billing.RootConfigManage)), mustPermission(t, string(billing.RootAdminRateLimitsUnlock)))
		}, func(w *world) {
			w.deps.UserInfo = nil
			w.cfg.DestructiveActionLimits = billing.DestructiveActionLimitsConfig{ErasurePerHour: 2, ActorPerHour: 2, MerchantPerHour: 2}
		})
		fleet := quietFleet(t, a.world)
		token, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		users := make([]string, 5)
		for i := range users {
			users[i] = uuid.NewString()
			status, raw := a.request(t, "POST", scimUsers, token.Token, scimUser(users[i], "user-"+users[i][:8]))
			require.Equal(t, http.StatusCreated, status, string(raw))
		}
		erasure := func(target string) (issuer, actor, state string) {
			t.Helper()
			require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT actor_issuer, actor_id, state FROM "+pgx.Identifier{a.schema, "destructive_actions"}.Sanitize()+" WHERE merchant_id = $1 AND kind = 'erase_contact' AND target = $2 ORDER BY admission_sequence DESC LIMIT 1",
				a.alpha.MerchantID().UUID(), target).Scan(&issuer, &actor, &state))
			return issuer, actor, state
		}
		present := func(id string, want int) {
			t.Helper()
			status, raw := a.request(t, "GET", scimUsers+"/"+id, token.Token, nil)
			require.Equal(t, want, status, string(raw))
		}
		contactName := func(id string) *string {
			t.Helper()
			customer, err := a.alpha.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(id)))
			require.NoError(t, err, "an erasure keeps the customer's billing")
			if customer.Contact == nil {
				return nil
			}
			return customer.Contact.Name
		}

		// Two erasures fit the budget: each runs at once and names the token.
		for _, id := range users[:2] {
			status, raw := a.request(t, "DELETE", scimUsers+"/"+id, token.Token, nil)
			require.Equal(t, http.StatusNoContent, status, string(raw))
			present(id, http.StatusNotFound)
			issuer, actor, state := erasure(id)
			require.Equal(t, httproutes.ProvisioningIssuer, issuer)
			require.Equal(t, token.ID.String(), actor)
			require.Equal(t, "succeeded", state)
			require.Nil(t, contactName(id))
		}
		// Each erasure that ran at once left its own drain queued, to run it
		// again had the run not finished.
		drains, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(destructive.DrainErasuresKind).States(rivertype.JobStateScheduled))
		require.NoError(t, err)
		require.Len(t, drains.Jobs, 2, "each started erasure queues its own drain")

		// The third waits: the User is gone from the directory at once, its
		// values stay, and the merchant's erasure hold opens.
		status, raw := a.request(t, "DELETE", scimUsers+"/"+users[2], token.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		present(users[2], http.StatusNotFound)
		_, _, state := erasure(users[2])
		require.Equal(t, "reserved", state)
		require.Nil(t, contactName(users[2]), "a waiting erasure takes the contact at once")
		found, err := a.alpha.ListCustomers(t.Context(), billing.CustomerListParams{Search: "user-" + users[2][:8]})
		require.NoError(t, err)
		require.Empty(t, found.Items, "a waiting erasure's contact is found by no search")
		holds, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
		require.NoError(t, err)
		require.Len(t, holds.Items, 1)
		hold := holds.Items[0]
		require.Equal(t, "erasure", hold.Scope)
		require.Equal(t, "erase_contact", hold.Action)
		require.EqualValues(t, 2, hold.Observed)
		require.EqualValues(t, 1, hold.Requested)
		require.EqualValues(t, 2, hold.Ceiling)
		trigger, target, _ := holdEvidence(t, a, hold)
		require.Equal(t, httproutes.ProvisioningIssuer, trigger.Issuer)
		require.Equal(t, token.ID.String(), trigger.ID)
		require.Equal(t, users[2], target)
		// A new token is under the same merchant hold: its erasure waits too.
		rotated, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "rotated"})
		require.NoError(t, err)
		status, raw = a.request(t, "DELETE", scimUsers+"/"+users[3], rotated.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		_, _, state = erasure(users[3])
		require.Equal(t, "reserved", state)

		// The erasure hold freezes no refund or revocation, and the erasures
		// counted toward no other budget: two actors' revocations fit the
		// merchant's limit of two, and the third opens the merchant's hold.
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		var in []billing.CreateCreditGrantParams
		for i := range 3 {
			in = append(in, billing.CreateCreditGrantParams{CustomerID: customer, Currency: "USD", Amount: 100, Source: "critical", SourceID: "erasure-scope-" + strconv.Itoa(i)})
		}
		grants, err := a.alpha.CreateCreditGrants(t.Context(), in)
		require.NoError(t, err)
		for i, grant := range grants {
			staff := a.remote(t, a.alpha.MerchantID(), roleKey(t, a, "support-"+strconv.Itoa(i), billing.MerchantCustomerManage))
			_, err := staff.RevokeCreditGrant(t.Context(), "", grant.ID, billing.RevokeCreditGrantParams{Reason: "operator correction"})
			if i < 2 {
				require.NoError(t, err)
				continue
			}
			requireCode(t, err, billing.CodeRateLimitExceeded)
		}
		holds, err = a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
		require.NoError(t, err)
		require.Len(t, holds.Items, 2)

		// A person holding merchant:provisioning:manage alone clears the
		// erasure hold, and a configuration-only one cannot; the waiting
		// erasures then run, though the merchant's own hold stays open.
		clear := "/v1/merchant/erasure-holds/" + hold.ID.String() + "/clear"
		configOnly := consoleStaff(t, a, "config-person", billing.MerchantConfigManage)
		status, raw = a.request(t, "POST", clear, configOnly, billing.ClearDestructiveHoldParams{Reason: "reviewed"})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), `"code":"permission_required"`)
		provisioner := consoleStaff(t, a, "directory-person", billing.MerchantProvisioningManage)
		status, raw = a.request(t, "POST", clear, provisioner, billing.ClearDestructiveHoldParams{Reason: "the directory removed them"})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Eventually(t, func() bool {
			_, _, second := erasure(users[2])
			_, _, third := erasure(users[3])
			return second == "succeeded" && third == "succeeded"
		}, 30*time.Second, 100*time.Millisecond, "the queue drains once the hold is cleared")
		require.Nil(t, contactName(users[2]))
		issuer, actor, _ := erasure(users[3])
		require.Equal(t, httproutes.ProvisioningIssuer, issuer)
		require.Equal(t, rotated.ID.String(), actor)
		stored, err := a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{IDs: []billing.DestructiveHoldID{hold.ID}})
		require.NoError(t, err)
		require.NotNil(t, stored.Items[0].ClearedAt)

		// The next erasure past the budget opens the hold again; a root
		// operator holding root:admin-rate-limits:unlock clears it through the
		// hold route.
		status, raw = a.request(t, "DELETE", scimUsers+"/"+users[4], token.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		holds, err = a.alpha.ListDestructiveHolds(t.Context(), billing.DestructiveHoldListParams{})
		require.NoError(t, err)
		var again billing.DestructiveHold
		for _, h := range holds.Items {
			if h.Scope == "erasure" && h.ClearedAt == nil {
				again = h
			}
		}
		require.NotEqual(t, hold.ID, again.ID, "a second erasure hold")
		user := authtest.NewUser(t, a.auth)
		authtest.GrantRole(t, a.auth, iam.RootGroup(), iam.UserSubject(user.ID), operator)
		status, raw = a.request(t, "POST", "/v1/merchant/destructive-holds/"+again.ID.String()+"/clear", authtest.SignIn(t, a.auth, user).AccessToken, billing.ClearDestructiveHoldParams{Reason: "reviewed by the operator"})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Eventually(t, func() bool {
			_, _, state := erasure(users[4])
			return state == "succeeded"
		}, 30*time.Second, 100*time.Millisecond, "the queue drains once the operator clears the hold")
	})
}

// raiseRecommendation opens a finding of alpha's that recommends rec.
func raiseRecommendation(t *testing.T, a *authorityWorld, subject string, rec recommend.Recommendation) billing.FindingID {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
	require.NoError(t, err)
	var finding reconcile.FindingRecord
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
		finding, err = (&reconcile.PGStore{DB: database, Now: a.clock.Now}).RaiseFinding(ctx, reconcile.RaisedFinding{Type: "consistency.duplicate.ownership",
			SubjectKey: subject, Severity: reconcile.SeverityHigh, Evidence: map[string]any{recommend.EvidenceKey: rec.Map()}})
		return err
	}))
	return billing.FindingID(finding.ID)
}

// raiseProposal opens a pull proposal of alpha's, as a pull writes one, whose
// change is of kind and names nothing to change: the approve route decides on
// its kind before anything runs.
func raiseProposal(t *testing.T, a *authorityWorld, kind billing.ChangeKind) billing.FindingID {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
	require.NoError(t, err)
	var finding reconcile.FindingRecord
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
		finding, err = (&reconcile.PGStore{DB: database, Now: a.clock.Now}).UpsertFinding(ctx, uuid.New(), reconcile.Finding{Type: reconcile.FindingLocalActiveRemoteDead,
			SubjectKey: "subscription:" + uuid.NewString(), Severity: reconcile.SeverityHigh, Status: reconcile.FindingStatusRequiresReview,
			Change: &reconcile.Change{FindingChange: billing.FindingChange{Kind: kind}}})
		return err
	}))
	return billing.FindingID(finding.ID)
}
