package routes

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/http/handlers"
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
	application := declared(t, "POST /v1/app/orders")
	application.Permission = Pair{}
	provisioning := declared(t, "DELETE /v1/app/scim/v2/Users/{id}")
	provisioning.Permission = Pair{}
	merchant := declared(t, "GET /v1/admin/orders")
	merchant.Group, merchant.Level, merchant.Name, merchant.Doc, merchant.Args, merchant.Cache = Customer, "", "", "", nil, ""
	customer := declared(t, "GET /v1/me/orders")
	customer.Permission = needOrders
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
	unknown := declared(t, "GET /v1/admin/orders")
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

// Approving a finding's recommendation asks for staff routes that exist and
// need customer support's manage permission.
func TestFindingActionsNeedCustomerManage(t *testing.T) {
	for action, keys := range handlers.FindingActionRoutes {
		for _, key := range keys {
			if r := declared(t, key); r.Needs() != needAdminUpdate {
				t.Errorf("%s: %s checks %v, want %v", action, key, r.Needs().Names(), needAdminUpdate.Names())
			}
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
	sensitive := declared(t, "GET /v1/me/orders/{id}")
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
