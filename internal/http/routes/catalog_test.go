package routes

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/handlers"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

func declared(t *testing.T, key string) Route {
	t.Helper()
	r, ok := routeByKey(key)
	if !ok {
		t.Fatalf("%s is not in the catalog", key)
	}
	return r
}

// The catalog refuses a route on a tier that checks a permission but names
// none, and a route naming one its tier never checks.
func TestCatalogRefusesRouteWithoutPermission(t *testing.T) {
	application := declared(t, "POST /v1/app/invoices")
	application.Permission = Pair{}
	provisioning := declared(t, "DELETE /v1/app/scim/v2/Users/{id}")
	provisioning.Permission = Pair{}
	merchant := declared(t, "GET /v1/admin/invoices")
	merchant.Group, merchant.Level, merchant.Name, merchant.Doc, merchant.Args, merchant.Cache = Customer, "", "", "", nil, ""
	customer := declared(t, "GET /v1/me/invoices")
	customer.Permission = needInvoices
	for name, c := range map[string]struct {
		route Route
		want  string
	}{
		"application":  {application, "checks a permission"},
		"provisioning": {provisioning, "checks a permission"},
		"merchant":     {merchant, "checks a permission"},
		"customer":     {customer, "checks no permission"},
	} {
		err := c.route.validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %s: validate() = %v, want a refusal that %s", name, c.route.Key(), err, c.want)
		}
	}
	unknown := declared(t, "GET /v1/admin/invoices")
	unknown.Auth = "optional"
	if err := unknown.authority(); err == nil || !strings.Contains(err.Error(), "unknown auth tier") {
		t.Errorf("an unknown tier: authority() = %v", err)
	}
}

// Every catalog route keeps the rules, and every route on a tier that checks
// a permission names its pair.
func TestCatalogRoutesNameTheirPermission(t *testing.T) {
	for _, r := range Catalog() {
		if err := r.validate(); err != nil {
			t.Errorf("%s: %v", r.Key(), err)
		}
		if r.Auth == AuthMerchant || r.Auth == AuthApplication || r.Auth == AuthProvisioning {
			if names := r.Needs().Names(); len(names) != 2 {
				t.Errorf("%s: checks %v, want its merchant: and root: names", r.Key(), names)
			}
		}
	}
}

// Approving a finding's recommendation or a proposal's change asks for the
// staff routes whose effect it runs: every action and change kind is listed,
// and each route it names needs customer support's manage permission.
func TestStaffActionsNeedTheirRoutes(t *testing.T) {
	actions := map[string]bool{}
	for _, action := range recommend.Actions() {
		actions[action] = true
		if _, ok := handlers.FindingActionRoutes[action]; !ok {
			t.Errorf("recommendation action %s names no routes", action)
		}
	}
	kinds := map[billing.ChangeKind]bool{}
	for _, kind := range billing.ChangeKinds() {
		kinds[kind] = true
		if _, ok := handlers.ProposalChangeRoutes[kind]; !ok {
			t.Errorf("change kind %s names no routes", kind)
		}
	}
	var keys []string
	for action, routes := range handlers.FindingActionRoutes {
		if !actions[action] {
			t.Errorf("FindingActionRoutes lists %s, which no recommendation names", action)
		}
		keys = append(keys, routes...)
	}
	for kind, routes := range handlers.ProposalChangeRoutes {
		if !kinds[kind] {
			t.Errorf("ProposalChangeRoutes lists %s, which is no change kind", kind)
		}
		keys = append(keys, routes...)
	}
	for _, key := range keys {
		if r := declared(t, key); r.Needs() != needAdminUpdate {
			t.Errorf("%s checks %v, want %v", key, r.Needs().Names(), needAdminUpdate.Names())
		}
	}
	// Only the kinds that record what the provider already did and billed are
	// the configuration's alone; one that charges or moves a subscription's
	// price, billing or access is customer support's too.
	configOnly := []billing.ChangeKind{billing.ChangeCreateSubscription, billing.ChangeRecordCharge, billing.ChangeRecordRefund, billing.ChangeAdoptSubscription, billing.ChangeUpdateCard}
	for _, kind := range billing.ChangeKinds() {
		if routes := handlers.ProposalChangeRoutes[kind]; (len(routes) == 0) != slices.Contains(configOnly, kind) {
			t.Errorf("change kind %s names routes %v; configuration-only kinds are %v", kind, routes, configOnly)
		}
	}
}

// The catalog refuses a route other than a GET at a read level unless
// queryRoutes lists it, a sensitive route under a pay URL, and a provider
// callback outside the webhooks group or a webhook checked otherwise.
func TestCatalogRefusesMisdeclaredTiers(t *testing.T) {
	unlisted := declared(t, "POST /v1/merchant/psps/routing-preview")
	unlisted.Path = "/v1/merchant/psps/routing-dry-run"
	readWrite := declared(t, "POST /v1/merchant/psps")
	readWrite.Level = LevelRead
	sensitive := declared(t, "GET /v1/me/invoices/{id}")
	sensitive.Sensitive = true
	callback := declared(t, "POST /v1/webhooks/{rail}/{account_id}")
	callback.Group = Payment
	for name, c := range map[string]struct {
		route Route
		want  string
	}{
		"unlisted read":     {unlisted, "queryRoutes"},
		"write read level":  {readWrite, "queryRoutes"},
		"sensitive pay URL": {sensitive, "pay URL"},
		"callback group":    {callback, "signature"},
	} {
		if c.route.PayURL == "" && name == "sensitive pay URL" {
			t.Fatalf("%s is no longer served under a pay URL", c.route.Key())
		}
		err := c.route.validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %s: validate() = %v, want a refusal naming %q", name, c.route.Key(), err, c.want)
		}
	}
}

// Provisioning tokens grant SCIM's routes, so they need the provisioning pair,
// not the configuration's.
func TestProvisioningTokensNeedTheProvisioningPair(t *testing.T) {
	for key, want := range map[string]Pair{
		"GET /v1/merchant/provisioning-tokens":         needProvisioningRead,
		"POST /v1/merchant/provisioning-tokens":        needProvisioning,
		"DELETE /v1/merchant/provisioning-tokens/{id}": needProvisioning,
	} {
		if got := declared(t, key).Needs(); got != want {
			t.Errorf("%s checks %v, want %v", key, got.Names(), want.Names())
		}
	}
}

// knowsNone is an Authenticator that declares no permission.
type knowsNone struct{}

func (knowsNone) Authenticate(*http.Request) (billingauth.Verified, error) {
	return nil, billingauth.ErrUnauthenticated
}
func (knowsNone) RootScope(context.Context) (billingauth.Scope, error) {
	return billingauth.Scope{Authority: "https://issuer.invalid", ID: "root"}, nil
}
func (knowsNone) KnownPermission(string) bool { return false }

// A mount whose Auth knows no provisioning permission names why SCIM mounted.
func TestSCIMMountErrorNamesItsCause(t *testing.T) {
	defer func() {
		refused, ok := recover().(MountError)
		if !ok || !strings.Contains(refused.Reason, "RouteGroups.Programmatic") || !strings.Contains(refused.Reason, "Deps.UserInfo") {
			t.Fatalf("mounting SCIM without its permissions: %#v, want a MountError naming its cause", refused)
		}
	}()
	needs(declared(t, "POST /v1/app/scim/v2/Users"), knowsNone{})
}
