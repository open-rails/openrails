package routes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func route(t *testing.T, key string) Route {
	t.Helper()
	r, ok := routeByKey(key)
	require.True(t, ok, key)
	return r
}

func everyStaffRoute(groups ...Group) []Route {
	var out []Route
	for _, r := range Catalog() {
		if r.Staff() && inGroups(r, groups) {
			out = append(out, r)
		}
	}
	return out
}

// The most specific guard wins: a route's own, then its resource group's,
// then its level group's.
func TestResolveGuardsPrefersTheMostSpecific(t *testing.T) {
	refund, resume := route(t, "POST /v1/merchant/payments/{id}/refunds"), route(t, "POST /v1/merchant/subscriptions/{id}/resume")
	cancel, payments := route(t, "POST /v1/merchant/subscriptions/{id}/cancel"), route(t, "GET /v1/merchant/payments")
	psps := route(t, "GET /v1/merchant/psps")
	guard, err := ResolveGuards(everyStaffRoute(Merchant, MerchantConfig), map[GuardKey]string{
		StaffReadsKey: "read", StaffWritesKey: "write", MerchantConfigKey: "admin",
		ResourceKey(ResRefunds): "refunds", ResourceKey(ResSubscriptions): "subscriptions", RouteKey("CancelSubscription"): "cancel",
	})
	require.NoError(t, err)
	require.Equal(t, "refunds", guard(refund), "a resource group overrides its level")
	require.Equal(t, "subscriptions", guard(resume))
	require.Equal(t, "cancel", guard(cancel), "a route overrides its resource and its level")
	require.Equal(t, "read", guard(payments))
	require.Equal(t, "admin", guard(psps), "a configuration read is MerchantConfig's")
}

func TestResolveGuardsFailsClosed(t *testing.T) {
	staff, all := everyStaffRoute(Merchant), everyStaffRoute(Merchant, MerchantConfig)
	levels := map[GuardKey]string{StaffReadsKey: "read", StaffWritesKey: "write"}
	with := func(base map[GuardKey]string, k GuardKey, v string) map[GuardKey]string {
		out := map[GuardKey]string{k: v}
		for key, perm := range base {
			out[key] = perm
		}
		return out
	}
	for name, tc := range map[string]struct {
		mounted []Route
		guards  map[GuardKey]string
		err     string
	}{
		"a staff route no guard covers":    {staff, map[GuardKey]string{StaffReadsKey: "read"}, "guards none of"},
		"MerchantConfig without its guard": {all, levels, "openrails.MerchantConfig)"},
		"a guard for an unmounted group":   {staff, with(levels, MerchantConfigKey, "admin"), "openrails.MerchantConfig covers no mounted route"},
		"a resource no mounted route has":  {staff, with(levels, ResourceKey(ResPSPs), "psps"), "openrails.PSPs covers no mounted route"},
		"an unknown key":                   {staff, with(levels, "resource:typo", "x"), `"resource:typo" is not a RouteSet`},
		"an empty permission":              {staff, with(levels, ResourceKey(ResRefunds), " "), "openrails.Refunds guards with an empty permission"},
		"two resources of one route": {all, with(with(with(levels, MerchantConfigKey, "admin"), ResourceKey(ResCatalog), "catalog"), ResourceKey(ResRefunds), "refunds"),
			"openrails.Catalog and openrails.Refunds both guard ArchiveProduct"},
	} {
		_, err := ResolveGuards(tc.mounted, tc.guards)
		require.ErrorContains(t, err, tc.err, name)
	}

	// Naming the route settles a conflict of its resources.
	_, err := ResolveGuards(all, map[GuardKey]string{
		StaffReadsKey: "read", StaffWritesKey: "write", MerchantConfigKey: "admin",
		ResourceKey(ResCatalog): "catalog", ResourceKey(ResRefunds): "refunds", RouteKey("ArchiveProduct"): "archive",
	})
	require.NoError(t, err)
}

// GuardNames are the published RouteSets: three level groups, every resource
// group, one per staff route.
func TestGuardNames(t *testing.T) {
	tiers := map[int]int{}
	names := map[string]bool{}
	for _, g := range GuardNames() {
		tiers[g.Tier]++
		require.False(t, names[g.Name], "%s is published twice", g.Name)
		names[g.Name] = true
	}
	require.Equal(t, 3, tiers[TierLevel])
	require.Equal(t, len(Resources), tiers[TierResource])
	require.Equal(t, len(everyStaffRoute(Merchant, MerchantConfig)), tiers[TierRoute])
}
