package routes

import (
	"strings"
	"testing"
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
