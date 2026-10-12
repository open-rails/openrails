package routes

import (
	"fmt"
	"slices"

	"github.com/open-rails/openrails/billing"
)

// Pair is the two names of the permission a route checks, either of which
// admits its caller: Merchant held in the scope of the merchant the request
// acts on, Root in the Authenticator's root scope. A route about no one
// merchant names Root alone.
type Pair struct{ Merchant, Root billing.Permission }

// Names are the pair's names, Merchant's first.
func (p Pair) Names() []billing.Permission {
	var out []billing.Permission
	for _, name := range []billing.Permission{p.Merchant, p.Root} {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// check refuses a pair that is not one exported permission's names: a
// root: name of billing.Permissions and, but for a route about no one
// merchant, its merchant: twin.
func (p Pair) check() error {
	if p == (Pair{}) {
		return nil
	}
	exported := billing.Permissions()
	if p.Root.Persona() != "root" || !slices.Contains(exported, p.Root) {
		return fmt.Errorf("its root permission %q is not a root: name of billing.Permissions", p.Root)
	}
	if p.Merchant == "" {
		return nil
	}
	if p.Merchant.Persona() != billing.MerchantGroupPersona || !slices.Contains(exported, p.Merchant) ||
		p.Merchant.Resource() != p.Root.Resource() || p.Merchant.Action() != p.Root.Action() {
		return fmt.Errorf("its permissions %q and %q are not one permission's merchant: and root: names", p.Merchant, p.Root)
	}
	return nil
}

// The staff groups' pairs, and the programmatic routes' own (Route.Permission).
var (
	needAdminRead      = Pair{billing.MerchantCustomerRead, billing.RootCustomerRead}
	needAdminUpdate    = Pair{billing.MerchantCustomerManage, billing.RootCustomerManage}
	needCatalog        = Pair{billing.MerchantCatalogManage, billing.RootCatalogManage}
	needMerchantConfig = Pair{billing.MerchantConfigManage, billing.RootConfigManage}
	needMetrics        = Pair{billing.MerchantMetricsRead, billing.RootMetricsRead}

	needEntitlements = Pair{billing.MerchantEntitlementsRead, billing.RootEntitlementsRead}
	needOffers       = Pair{billing.MerchantCatalogRead, billing.RootCatalogRead}
	needUsageRead    = Pair{billing.MerchantUsageRead, billing.RootUsageRead}
	needUsage        = Pair{billing.MerchantUsageManage, billing.RootUsageManage}
	needEvents       = Pair{billing.MerchantEventsRead, billing.RootEventsRead}
	needOrders       = Pair{billing.MerchantOrdersManage, billing.RootOrdersManage}
	needCustomers    = Pair{billing.MerchantCustomersRead, billing.RootCustomersRead}
)

// Needs is the pair a route checks: a staff route's group's (and level's), a
// programmatic route's own, none for any other route.
func (r Route) Needs() Pair {
	switch r.Group {
	case CatalogAdmin:
		return needCatalog
	case MerchantConfig:
		return needMerchantConfig
	case Metrics:
		return needMetrics
	case Admin:
		if r.Level == LevelUpdate {
			return needAdminUpdate
		}
		return needAdminRead
	case App:
		return r.Permission
	}
	return Pair{}
}
