package catalogpublish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/open-rails/openrails"
	billingservice "github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// plan computes the convergence diff for a manifest against the catalog exposed
// by applier. It performs only reads (GetProductByKey, ListProducts,
// ListPricesByProduct).
func plan(ctx context.Context, applier applier, m *Manifest) (*ApplyPlan, error) {
	return planWithOptions(ctx, applier, m, PlanOptions{ArchiveMissingProducts: true, ArchiveMissingPrices: true})
}

// planWithOptions computes the convergence diff using explicit reconciliation
// semantics.
func planWithOptions(ctx context.Context, applier applier, m *Manifest, opts PlanOptions) (*ApplyPlan, error) {
	plan := &ApplyPlan{}
	for _, group := range m.TierGroups {
		gp := GroupPlan{Key: group.Key}

		declared := make(map[string]struct{}, len(group.Products))
		for _, product := range group.Products {
			declared[product.Key] = struct{}{}
			pp, err := planProduct(ctx, applier, m, group, product, opts)
			if err != nil {
				return nil, err
			}
			gp.Products = append(gp.Products, *pp)
		}

		if opts.ArchiveMissingProducts {
			// Products dropped from the manifest -> archive. Scope to active products
			// in this tier group.
			live := false
			for offset := 0; ; {
				page, err := applier.ListProducts(ctx, billingservice.ListProductsOptions{Archived: &live, TierGroup: group.Key, Limit: 1000, Offset: offset})
				if err != nil {
					return nil, fmt.Errorf("list active products for tier group %s: %w", group.Key, err)
				}
				for _, product := range page.Items {
					if _, ok := declared[product.Key]; !ok {
						gp.RemovedProducts = append(gp.RemovedProducts, product)
					}
				}
				next := page.Offset + page.Limit
				if int64(next) >= page.Total {
					break
				}
				if len(page.Items) == 0 || next <= offset {
					return nil, fmt.Errorf("catalog pagination made no progress for tier group %s", group.Key)
				}
				offset = next
			}
		}

		plan.Groups = append(plan.Groups, gp)
	}
	return plan, nil
}

func planProduct(ctx context.Context, applier applier, m *Manifest, group TierGroup, product Product, opts PlanOptions) (*ProductPlan, error) {
	entitlements := entitlementsSpec(product.Entitlements)
	// Usage-metered products carry no tier_group — they aren't tier-exclusive
	// subscriptions (#642). The loader put them in a synthetic singleton group;
	// persist NULL so they never share tier exclusivity.
	tierGroupPtr := &group.Key
	if len(product.RateCards) > 0 {
		tierGroupPtr = nil
	}
	tierRank := declaredTierRank(product)

	pp := &ProductPlan{Key: product.Key}

	existing, err := applier.GetProductByKey(ctx, product.Key)
	if err != nil && !errors.Is(err, openrails.ErrNotFound) {
		return nil, fmt.Errorf("look up catalog product %q: %w", product.Key, err)
	}
	if err == nil && existing == nil {
		return nil, fmt.Errorf("look up catalog product %q returned no result", product.Key)
	}
	if err != nil {
		// Only an absent product authorizes a create plan. A failed read does
		// not establish absence and must not produce a successful plan.
		pp.Action = ProductCreate
		pp.CreateReq = billingservice.CreateProductRequest{
			Key:              product.Key,
			DisplayName:      product.DisplayName,
			Description:      strings.TrimSpace(product.Description),
			EntitlementsSpec: entitlements,
			TierGroup:        tierGroupPtr,
			TierRank:         tierRank,
			Archived:         product.Archived,
		}
		if err := planPrices(ctx, applier, m, product, nil, pp, opts); err != nil {
			return nil, err
		}
		return pp, nil
	}

	pp.UpdateID = existing.ID
	name := product.DisplayName
	desc := strings.TrimSpace(product.Description)
	archived := product.Archived
	pp.UpdateReq = billingservice.UpdateProductRequest{
		DisplayName:      &name,
		Description:      &desc,
		EntitlementsSpec: entitlements,
		SetEntitlements:  true,
		TierGroup:        tierGroupPtr,
		SetTierGroup:     true,
		TierRank:         &tierRank,
		Archived:         &archived,
	}
	if productUnchanged(existing, product, entitlements, tierGroupPtr, tierRank) {
		pp.Action = ProductUnchanged
	} else {
		pp.Action = ProductUpdate
	}

	if err := planPrices(ctx, applier, m, product, existing, pp, opts); err != nil {
		return nil, err
	}
	return pp, nil
}

// sameTierGroup compares the persisted tier_group against the desired one, both
// nullable: a usage product persists NULL (#642), a subscription product a slug.
func sameTierGroup(existing, desired *string) bool {
	if existing == nil || desired == nil {
		return existing == nil && desired == nil
	}
	return strings.EqualFold(strings.TrimSpace(*existing), strings.TrimSpace(*desired))
}

func productUnchanged(existing *billingservice.CatalogProduct, product Product, entitlements map[string]*int, tierGroup *string, tierRank int) bool {
	if existing == nil {
		return false
	}
	if existing.DisplayName != product.DisplayName {
		return false
	}
	if strings.TrimSpace(existing.Description) != strings.TrimSpace(product.Description) {
		return false
	}
	if existing.TierRank != tierRank {
		return false
	}
	if !sameTierGroup(existing.TierGroup, tierGroup) {
		return false
	}
	if existing.Archived != product.Archived {
		return false
	}
	if len(existing.EntitlementsSpec) != len(entitlements) {
		return false
	}
	for k := range entitlements {
		if _, ok := existing.EntitlementsSpec[k]; !ok {
			return false
		}
	}
	return true
}

// planPrices reconciles the declared prices for a product as a SET. Each
// declared price -> create (if no financial match) or ensure-status (if
// matched). Any ACTIVE OpenRails price whose financial identity is not declared
// -> archive. existing is the matched OpenRails product (nil when the product
// is being created, in which case no OpenRails prices can exist yet).
func planPrices(ctx context.Context, applier applier, m *Manifest, product Product, existing *billingservice.CatalogProduct, pp *ProductPlan, opts PlanOptions) error {
	var current []billingservice.CatalogPrice
	if existing != nil && !existing.ID.IsZero() {
		var err error
		current, err = applier.ListPricesByProduct(ctx, existing.ID, false)
		if err != nil {
			return fmt.Errorf("list prices for product %s: %w", product.Key, err)
		}
	}

	claimed := map[openrails.PriceID]struct{}{}

	// #774: resolve every declared price's key (explicit or auto-defaulted
	// "<product-key>-<interval>") up front, over the WHOLE declared set, so an
	// ambiguity — two or more declared prices resolving to the identical key,
	// almost always two undeclared prices sharing an interval (a promo
	// alongside the standard price) — is refused loudly at PLAN time, before
	// anything is written. Collision detection is a plan-time concern
	// precisely because it needs the full declared set; the imperative
	// CreatePrice API (billingservice.Service, MODE 2/console) sees one price
	// at a time and always resolves a concrete key.
	resolvedKeys := make([]string, len(product.Prices))
	byResolvedKey := map[string][]string{}
	for i, price := range product.Prices {
		accessDurationHours, err := normalizeDuration(price.Duration)
		if err != nil {
			return apperr.Invalidf("price %s duration: %v", PriceLabel(product.Key, price), err).WithParam("duration")
		}
		key := strings.TrimSpace(price.Key)
		if key == "" {
			key = product.Key + "-" + billingservice.PriceIntervalLabel(accessDurationHours, price.AutoRenew)
		}
		resolvedKeys[i] = key
		byResolvedKey[key] = append(byResolvedKey[key], PriceLabel(product.Key, price))
	}
	for key, labels := range byResolvedKey {
		if len(labels) > 1 {
			return apperr.Invalidf("product %q: prices [%s] all resolve to price key %q — set an explicit `key:` on each price to disambiguate", product.Key, strings.Join(labels, ", "), key).WithParam("key")
		}
	}

	for i, price := range product.Prices {
		accessDurationHours, err := normalizeDuration(price.Duration)
		if err != nil {
			return apperr.Invalidf("price %s duration: %v", PriceLabel(product.Key, price), err).WithParam("duration")
		}
		label := PriceLabel(product.Key, price)
		key := resolvedKeys[i]

		// #622 trial first phase (a different first-phase price/length) is part
		// of price identity, so normalize it up front and match on it too.
		var trialAmount *int64
		var trialHours *int
		if price.Trial != nil {
			amt := price.Trial.UnitAmount
			hours, err := normalizeDuration(price.Trial.Duration)
			if err != nil {
				return apperr.Invalidf("price %s trial.duration: %v", label, err).WithParam("trial.duration")
			}
			trialAmount = &amt
			trialHours = hours
		}

		match := matchPrice(current, price, accessDurationHours, trialHours, trialAmount, claimed)
		if match != nil {
			claimed[match.ID] = struct{}{}
			plp := PricePlan{Label: label, ExistingID: match.ID}
			switch {
			case match.Archived == price.Archived:
				plp.Action = PriceUnchanged
			case price.Archived: // archived desired but currently active
				plp.Action = PriceArchive
			default: // active desired but currently archived
				plp.Action = PriceActivate
			}
			// #774: a substance-unchanged price declared under a DIFFERENT key
			// is a plain rename — signal apply to relabel via SetPriceKey. Never
			// set for an unchanged key (nothing to do).
			if match.Key != key {
				plp.Key = key
			}
			plp.PSPLinks = pspLinksToRotate(price.PSPLinks, match.Providers)
			pp.Prices = append(pp.Prices, plp)
			continue
		}

		// No financial match -> create.
		createReq := billingservice.CreatePriceRequest{
			// ProductID is filled at apply time once the product exists.
			ProductID:           productID(existing),
			Key:                 key,
			UnitAmount:          price.UnitAmount,
			Currency:            price.Currency,
			AccessDurationHours: accessDurationHours,
			AutoRenew:           price.AutoRenew,
			TrialUnitAmount:     trialAmount,
			TrialDurationHours:  trialHours,
			PSPs:                price.PSPs,
			PSPLinks:            price.PSPLinks,
			Archived:            price.Archived,
		}
		pp.Prices = append(pp.Prices, PricePlan{
			Label:     label,
			Action:    PriceCreate,
			CreateReq: createReq,
			Key:       key,
		})
	}

	if opts.ArchiveMissingPrices {
		// Archive any ACTIVE OpenRails price not claimed by a declared price.
		for i := range current {
			c := &current[i]
			if _, ok := claimed[c.ID]; ok {
				continue
			}
			if c.Archived {
				continue
			}
			pp.Prices = append(pp.Prices, PricePlan{
				Label:      PriceLabel(product.Key, Price{Currency: c.Currency, UnitAmount: c.UnitAmount}),
				Action:     PriceArchive,
				ExistingID: c.ID,
			})
		}
	}
	return nil
}

// pspLinksToRotate returns the declared psp_links entries a matched price's
// stored links do not satisfy. An entry is satisfied when every declared
// key/value is present verbatim (trimmed) on the stored link for that PSP;
// extra stored keys (provider-generated ids such as plan_pda or mint_symbol)
// never count as drift. Nil when nothing needs rotating.
func pspLinksToRotate(declared map[string]map[string]string, current map[string]billingservice.ProviderState) map[string]map[string]string {
	var out map[string]map[string]string
	for psp, link := range declared {
		psp = strings.ToLower(strings.TrimSpace(psp))
		if psp == "" || len(link) == 0 {
			continue
		}
		stored := current[psp].IDs
		satisfied := true
		for k, v := range link {
			if strings.TrimSpace(stored[strings.TrimSpace(k)]) != strings.TrimSpace(v) {
				satisfied = false
				break
			}
		}
		if satisfied {
			continue
		}
		if out == nil {
			out = map[string]map[string]string{}
		}
		out[psp] = maps.Clone(link)
	}
	return out
}

// matchPrice finds an existing OpenRails price with the same financial identity
// as the declared price, preferring an unclaimed active match over an archived
// one. Identity is exactly the unique_prices_product_amount_window key:
// (currency, unit_amount, access_duration_hours, auto_renew, trial_unit_amount,
// trial_duration_hours). PSPs are NOT part of identity — the DB constraint
// forbids two prices that differ only by provider, so a provider-set drift is a
// mutation of the matched price, never a reason to create a second row (doing so
// collides on the unique key).
func matchPrice(current []billingservice.CatalogPrice, price Price, accessDurationHours, trialHours *int, trialAmount *int64, claimed map[openrails.PriceID]struct{}) *billingservice.CatalogPrice {
	var best *billingservice.CatalogPrice
	for i := range current {
		c := &current[i]
		if _, ok := claimed[c.ID]; ok {
			continue
		}
		if c.UnitAmount != price.UnitAmount || !strings.EqualFold(c.Currency, price.Currency) {
			continue
		}
		if !sameCycleDays(c.AccessDurationHours, accessDurationHours) || c.AutoRenew != price.AutoRenew {
			continue
		}
		// Trial is part of identity (NULLS NOT DISTINCT): "no trial" is a concrete
		// value, so nil==nil but nil!=set.
		if !samePtrInt64(c.TrialUnitAmount, trialAmount) || !samePtrInt(c.TrialDurationHours, trialHours) {
			continue
		}
		if best == nil || (best.Archived && !c.Archived) {
			best = c
		}
	}
	return best
}

func sameCycleDays(a, b *int) bool {
	if a == nil || *a <= 0 {
		return b == nil || *b <= 0
	}
	return b != nil && *a == *b
}

// samePtrInt / samePtrInt64 mirror the DB's NULLS NOT DISTINCT comparison: two
// NULLs are equal, a NULL and a value are not.
func samePtrInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func samePtrInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func productID(p *billingservice.CatalogProduct) openrails.ProductID {
	if p == nil {
		return openrails.ProductID{}
	}
	return p.ID
}

func entitlementsSpec(entitlements []string) map[string]*int {
	out := map[string]*int{}
	for _, e := range entitlements {
		e = strings.TrimSpace(e)
		if e != "" {
			out[e] = nil
		}
	}
	return out
}

// String renders the plan as a terraform-style change log.
func planString(plan *ApplyPlan) string {
	var b strings.Builder
	if plan.MetersChanged {
		fmt.Fprintln(&b, "~ meter definitions")
	}
	if plan.RateCardsChanged {
		fmt.Fprintln(&b, "~ rate-card definitions")
	}
	for gi := range plan.Groups {
		gp := &plan.Groups[gi]
		fmt.Fprintf(&b, "tier_group %s\n", gp.Key)
		for pi := range gp.Products {
			pp := &gp.Products[pi]
			fmt.Fprintf(&b, "  %s product %s\n", symbol(string(pp.Action)), pp.Key)
			var changes []string
			for i := range pp.Prices {
				plp := &pp.Prices[i]
				line := fmt.Sprintf("%s %s", plp.Action, plp.Label)
				if len(plp.PSPLinks) > 0 {
					psps := slices.Sorted(maps.Keys(plp.PSPLinks))
					line += fmt.Sprintf(" (rotate psp_links: %s)", strings.Join(psps, ", "))
				}
				changes = append(changes, line)
			}
			sort.Strings(changes)
			for _, c := range changes {
				fmt.Fprintf(&b, "      %s\n", c)
			}
		}
		for i := range gp.RemovedProducts {
			fmt.Fprintf(&b, "  - product %s (archived: removed from manifest)\n", gp.RemovedProducts[i].Key)
		}
	}
	return b.String()
}

// Print writes the plan to out, with an optional dry-run banner.
func PrintPlan(plan *ApplyPlan, out io.Writer, dryRun bool) {
	if dryRun {
		fmt.Fprintln(out, "catalog plan (dry run; no changes applied):")
	} else {
		fmt.Fprintln(out, "catalog plan:")
	}
	fmt.Fprint(out, planString(plan))
}

func symbol(action string) string {
	switch action {
	case string(ProductCreate):
		return "+"
	case string(ProductUpdate):
		return "~"
	case string(ProductArchive):
		return "-"
	default:
		return " "
	}
}

// PriceLabel derives a readable label for a price from its financial terms,
// e.g. "starter $13.00/month". There is no price slug to lean on.
func PriceLabel(productKey string, price Price) string {
	duration := strings.TrimSpace(price.Duration)
	if duration == "" {
		duration = "indefinite"
	}
	money := formatMoney(price.UnitAmount, price.Currency)
	if price.AutoRenew {
		return fmt.Sprintf("%s %s/%s", productKey, money, duration)
	}
	if duration == "indefinite" {
		return fmt.Sprintf("%s %s once", productKey, money)
	}
	return fmt.Sprintf("%s %s for %s", productKey, money, duration)
}

// formatMoney renders an internal micros amount as a human currency string.
func formatMoney(unitAmount int64, currency string) string {
	amount := moneyutil.FormatMicrosDecimal(moneyutil.Micros(unitAmount))
	if currency == "" || strings.EqualFold(currency, "usd") {
		return "$" + amount
	}
	return strings.ToUpper(currency) + " " + amount
}
