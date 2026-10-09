package routes

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/app"
)

// GuardKey names a Routes.Guards entry: a level group, a resource group or
// one route. openrails.RouteSet's constants are generated from GuardKeys.
type GuardKey string

// The level groups.
const (
	StaffReadsKey     GuardKey = "staff_reads"
	StaffWritesKey    GuardKey = "staff_writes"
	MerchantConfigKey GuardKey = "merchant_config"
)

// ResourceKey is a resource group's key.
func ResourceKey(r Resource) GuardKey { return GuardKey("resource:" + string(r)) }

// RouteKey is one route's key, by its Go client method.
func RouteKey(name string) GuardKey { return GuardKey("route:" + name) }

// Guard tiers, most specific first.
const (
	TierRoute = iota
	TierResource
	TierLevel
)

// GuardName is a key as openrails publishes it.
type GuardName struct {
	Key  GuardKey
	Name string
	Tier int
	// Routes are the keys of the routes it covers.
	Routes []string
}

// levelKey is the level group a staff route belongs to.
func (r Route) levelKey() GuardKey {
	switch {
	case r.Group == MerchantConfig:
		return MerchantConfigKey
	case r.Level == LevelWrite:
		return StaffWritesKey
	}
	return StaffReadsKey
}

// Guards are the names of the guards covering a staff route, most specific
// first: its route, its resource groups, its level group.
func (r Route) Guards() []string {
	if !r.Staff() {
		return nil
	}
	out := []string{r.Name}
	for _, res := range r.Resources {
		out = append(out, Resources[res])
	}
	switch r.levelKey() {
	case MerchantConfigKey:
		return append(out, "MerchantConfig")
	case StaffWritesKey:
		return append(out, "StaffWrites")
	}
	return append(out, "StaffReads")
}

// guardTiers are the keys covering a staff route, by tier.
func (r Route) guardTiers() [3][]GuardKey {
	var resources []GuardKey
	for _, res := range r.Resources {
		resources = append(resources, ResourceKey(res))
	}
	return [3][]GuardKey{{RouteKey(r.Name)}, resources, {r.levelKey()}}
}

// GuardNames is every key, ordered by tier then name.
func GuardNames() []GuardName {
	byKey := map[GuardKey]*GuardName{
		StaffReadsKey:     {Key: StaffReadsKey, Name: "StaffReads", Tier: TierLevel},
		StaffWritesKey:    {Key: StaffWritesKey, Name: "StaffWrites", Tier: TierLevel},
		MerchantConfigKey: {Key: MerchantConfigKey, Name: "MerchantConfig", Tier: TierLevel},
	}
	for res, name := range Resources {
		byKey[ResourceKey(res)] = &GuardName{Key: ResourceKey(res), Name: name, Tier: TierResource}
	}
	for _, r := range Catalog() {
		if !r.Staff() {
			continue
		}
		byKey[RouteKey(r.Name)] = &GuardName{Key: RouteKey(r.Name), Name: r.Name, Tier: TierRoute}
		for _, tier := range r.guardTiers() {
			for _, k := range tier {
				byKey[k].Routes = append(byKey[k].Routes, r.Key())
			}
		}
	}
	out := make([]GuardName, 0, len(byKey))
	for _, g := range byKey {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier > out[j].Tier
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ResolveGuards is the permission each mounted staff route checks: its
// route's guard, else its resource group's, else its level group's. It
// refuses an unknown key or an empty permission, a key that covers no
// mounted route, two keys of the deciding tier, and a route no key covers:
// nothing is mounted open by omission or by a typo.
func ResolveGuards(mounted []Route, guards map[GuardKey]string) (func(Route) string, error) {
	names := map[GuardKey]string{}
	for _, g := range GuardNames() {
		names[g.Key] = "openrails." + g.Name
	}
	for _, k := range sortedKeys(guards) {
		name, ok := names[k]
		if !ok {
			return nil, fmt.Errorf("openrails: Routes.Guards: %q is not a RouteSet", string(k))
		}
		if strings.TrimSpace(guards[k]) == "" {
			return nil, fmt.Errorf("openrails: Routes.Guards: %s guards with an empty permission", name)
		}
	}
	covers := map[GuardKey]bool{}
	resolved := map[string]string{}
	var uncovered []string
	for _, r := range mounted {
		if !r.Staff() {
			continue
		}
		tiers := r.guardTiers()
		for _, tier := range tiers {
			for _, k := range tier {
				covers[k] = true
			}
		}
		decided := false
		for _, tier := range tiers {
			var hits []GuardKey
			for _, k := range tier {
				if _, ok := guards[k]; ok {
					hits = append(hits, k)
				}
			}
			if len(hits) > 1 {
				return nil, fmt.Errorf("openrails: Routes.Guards: %s and %s both guard %s (%s); guard the route itself with openrails.%s", names[hits[0]], names[hits[1]], r.Name, r.Key(), r.Name)
			}
			if len(hits) == 1 {
				resolved[r.Key()], decided = guards[hits[0]], true
				break
			}
		}
		if !decided {
			uncovered = append(uncovered, fmt.Sprintf("%s (%s)", r.Key(), names[r.levelKey()]))
		}
	}
	if len(uncovered) > 0 {
		sort.Strings(uncovered)
		return nil, fmt.Errorf("openrails: Routes.Guards guards none of %d mounted staff routes: %s", len(uncovered), strings.Join(uncovered, ", "))
	}
	for _, k := range sortedKeys(guards) {
		if !covers[k] {
			return nil, fmt.Errorf("openrails: Routes.Guards: %s covers no mounted route", names[k])
		}
	}
	return func(r Route) string { return resolved[r.Key()] }, nil
}

func sortedKeys(guards map[GuardKey]string) []GuardKey {
	out := make([]GuardKey, 0, len(guards))
	for k := range guards {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PlanStaffRoutes is the staff routes of groups that opts would mount on
// rt: what ResolveGuards resolves.
func PlanStaffRoutes(rt *app.Runtime, opts Options, groups ...Group) []Route {
	e := newEnv(rt, opts)
	var out []Route
	for _, r := range Catalog() {
		if r.Staff() && inGroups(r, groups) && e.mounts(r) {
			out = append(out, r)
		}
	}
	return out
}

func inGroups(r Route, groups []Group) bool {
	for _, g := range groups {
		if r.Group == g {
			return true
		}
	}
	return false
}
