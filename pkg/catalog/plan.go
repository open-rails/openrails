package catalog

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// ProductAction is the per-product change a plan records.
type ProductAction string

const (
	ProductCreate    ProductAction = "create"
	ProductUpdate    ProductAction = "update"
	ProductUnchanged ProductAction = "unchanged"
	ProductArchive   ProductAction = "archive" // active in OpenRails, removed from manifest
)

// PriceAction is the per-price change a plan records.
type PriceAction string

const (
	PriceCreate    PriceAction = "create"
	PriceUpdate    PriceAction = "update"
	PriceActivate  PriceAction = "activate"
	PriceArchive   PriceAction = "archive"
	PriceUnchanged PriceAction = "unchanged"
)

// ApplyPlan is the full terraform-style diff between the manifest (desired) and
// OpenRails (current). It is computed without mutating anything, printed, and
// then converged by Apply.
type ApplyPlan struct {
	Groups   []GroupPlan `json:"groups"`
	Manifest *Manifest   `json:"-"`
}

// PlanOptions controls how the manifest is compared to the live catalog.
type PlanOptions struct {
	// ArchiveMissingProducts archives active products in a declared tier group
	// when they are absent from the manifest. The push-catalog command keeps this
	// terraform-style convergence behavior.
	ArchiveMissingProducts bool
	// ArchiveMissingPrices archives active prices for a declared product when
	// their financial identity is absent from the manifest.
	ArchiveMissingPrices bool
}

// GroupPlan is the diff for one tier group.
type GroupPlan struct {
	Key      string        `json:"key"`
	Products []ProductPlan `json:"products"`
	// RemovedProducts are active OpenRails products in this tier group not
	// declared in the manifest; they are archived (deactivated) on apply.
	RemovedProducts []billingservice.CatalogProduct `json:"removed_products,omitempty"`
}

// ProductPlan is the diff for one product plus its price set.
type ProductPlan struct {
	Key    string        `json:"key"`
	Action ProductAction `json:"action"`

	// CreateReq / UpdateReq / UpdateID are prepared for apply.
	CreateReq billingservice.CreateProductRequest `json:"create_req,omitempty"`
	UpdateReq billingservice.UpdateProductRequest `json:"update_req,omitempty"`
	UpdateID  uuid.UUID                           `json:"update_id,omitempty"`

	Prices []PricePlan `json:"prices,omitempty"`
}

// PricePlan is the diff for one price (identity = financial substance).
type PricePlan struct {
	Label  string      `json:"label"`
	Action PriceAction `json:"action"`

	// ExistingID is the matched OpenRails price (uuid.Nil when creating).
	ExistingID uuid.UUID                         `json:"existing_id,omitempty"`
	CreateReq  billingservice.CreatePriceRequest `json:"create_req,omitempty"`
	UpdateReq  billingservice.UpdatePriceRequest `json:"update_req,omitempty"`

	// Key (#774) is this declared price's resolved key (explicit or
	// auto-defaulted). For Action==PriceCreate it rides CreateReq.Key; for a
	// MATCHED price (Unchanged/Activate/Archive) it is set ONLY when it
	// differs from the matched row's current key — signaling apply must
	// relabel (a plain key rename, no substance change) via SetPriceKey.
	Key string `json:"key,omitempty"`
}

// Plan computes the convergence diff for a manifest against the catalog exposed
// by applier. It performs only reads (GetProductByKey, ListProducts,
// ListPricesByProduct).
func Plan(ctx context.Context, applier Applier, m *Manifest) (*ApplyPlan, error) {
	return PlanWithOptions(ctx, applier, m, PlanOptions{ArchiveMissingProducts: true, ArchiveMissingPrices: true})
}

// PlanWithOptions computes the convergence diff using explicit reconciliation
// semantics.
func PlanWithOptions(ctx context.Context, applier Applier, m *Manifest, opts PlanOptions) (*ApplyPlan, error) {
	plan := &ApplyPlan{Manifest: m}
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
			existing, _, err := applier.ListProducts(ctx, billingservice.ListProductsOptions{
				ActiveOnly: true,
				TierGroup:  group.Key,
				Limit:      1000,
			})
			if err != nil {
				return nil, fmt.Errorf("list active products for tier group %s: %w", group.Key, err)
			}
			for i := range existing {
				if _, ok := declared[existing[i].Key]; ok {
					continue
				}
				gp.RemovedProducts = append(gp.RemovedProducts, existing[i])
			}
		}

		plan.Groups = append(plan.Groups, gp)
	}
	return plan, nil
}

func planProduct(ctx context.Context, applier Applier, m *Manifest, group TierGroup, product Product, opts PlanOptions) (*ProductPlan, error) {
	entitlements := entitlementsSpec(product.Entitlements)
	credits := creditsSpec(product.Credits)
	// Usage-metered products carry no tier_group — they aren't tier-exclusive
	// subscriptions (#642). The loader put them in a synthetic singleton group;
	// persist NULL so they never share tier exclusivity.
	tierGroupPtr := &group.Key
	if len(product.RateCards) > 0 {
		tierGroupPtr = nil
	}
	tierRank := product.tierRank()

	pp := &ProductPlan{Key: product.Key}

	existing, err := applier.GetProductByKey(ctx, product.Key)
	if err != nil || existing == nil {
		// Not found -> create. (The facade returns an error for "not found"; we
		// treat any lookup failure as create-intent — apply will surface a real
		// create error if the slug actually exists.)
		pp.Action = ProductCreate
		pp.CreateReq = billingservice.CreateProductRequest{
			Key:              product.Key,
			DisplayName:      product.DisplayName,
			Description:      strings.TrimSpace(product.Description),
			EntitlementsSpec: entitlements,
			CreditsSpec:      credits,
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
		CreditsSpec:      credits,
		SetCredits:       true,
		TierGroup:        tierGroupPtr,
		SetTierGroup:     true,
		TierRank:         &tierRank,
		Archived:         &archived,
	}
	if productUnchanged(existing, product, entitlements, credits, tierGroupPtr, tierRank) {
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

func productUnchanged(existing *billingservice.CatalogProduct, product Product, entitlements map[string]*int, credits billingservice.CreditsSpec, tierGroup *string, tierRank int) bool {
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
	if len(existing.CreditsSpec) != len(credits) {
		return false
	}
	for k, v := range credits {
		ev, ok := existing.CreditsSpec[k]
		if !ok || ev.Unit != v.Unit || ev.Amount != v.Amount || ev.Cadence != v.Cadence {
			return false
		}
		if (ev.ExpiryHours == nil) != (v.ExpiryHours == nil) {
			return false
		}
		if ev.ExpiryHours != nil && *ev.ExpiryHours != *v.ExpiryHours {
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
func planPrices(ctx context.Context, applier Applier, m *Manifest, product Product, existing *billingservice.CatalogProduct, pp *ProductPlan, opts PlanOptions) error {
	var current []billingservice.CatalogPrice
	if existing != nil && existing.ID != uuid.Nil {
		var err error
		current, err = applier.ListPricesByProduct(ctx, existing.ID, false)
		if err != nil {
			return fmt.Errorf("list prices for product %s: %w", product.Key, err)
		}
	}

	claimed := map[uuid.UUID]struct{}{}

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
		if price.Model != "" {
			continue
		}
		accessDurationHours, err := normalizeDuration(price.Duration)
		if err != nil {
			return fmt.Errorf("price %s duration: %w", PriceLabel(product.Key, price), err)
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
			return fmt.Errorf("product %q: prices [%s] all resolve to price key %q — set an explicit `key:` on each price to disambiguate (#774)", product.Key, strings.Join(labels, ", "), key)
		}
	}

	for i, price := range product.Prices {
		if price.Model != "" {
			continue
		}
		accessDurationHours, err := normalizeDuration(price.Duration)
		if err != nil {
			return fmt.Errorf("price %s duration: %w", PriceLabel(product.Key, price), err)
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
				return fmt.Errorf("price %s trial.duration: %w", label, err)
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
			// Never plan link work for a price this run is archiving: syncing
			// links drives adapter.Attach, which can PUBLISH (e.g. a Solana
			// plan from token) — minting a live provider object for a
			// dead price. The declared links converge if it is ever unarchived.
			if !price.Archived {
				if links := pspLinksNeedingSync(match.Providers, price.PSPLinks); len(links) > 0 {
					plp.UpdateReq.PSPLinks = links
					if plp.Action == PriceUnchanged {
						plp.Action = PriceUpdate
					}
				}
			}
			// #774: a substance-unchanged price declared under a DIFFERENT key
			// is a plain rename — signal apply to relabel via SetPriceKey. Never
			// set for an unchanged key (nothing to do).
			if match.Key != key {
				plp.Key = key
			}
			pp.Prices = append(pp.Prices, plp)
			continue
		}

		// No financial match -> create.
		psps := price.PSPs
		pspLinks := price.PSPLinks
		if price.Archived {
			// Historical prices are local records only. Do not let CreatePrice
			// publish provider objects for a price that starts archived; the
			// declarations converge if the price is later activated.
			psps = nil
			pspLinks = nil
		}
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
			PSPs:                psps,
			PSPLinks:            pspLinks,
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

// pspLinksNeedingSync returns the desired provider link/config values that are
// not already present in the stored provider state. Generated provider fields
// may coexist with the desired subset and do not count as drift.
func pspLinksNeedingSync(
	current map[string]billingservice.ProviderState,
	desired map[string]map[string]string,
) map[string]map[string]string {
	var out map[string]map[string]string
	for rawPSP, rawLink := range desired {
		psp := strings.ToLower(strings.TrimSpace(rawPSP))
		if psp == "" {
			continue
		}
		link := make(map[string]string, len(rawLink))
		state := current[psp]
		needsSync := false
		for rawKey, rawValue := range rawLink {
			key := strings.TrimSpace(rawKey)
			value := strings.TrimSpace(rawValue)
			if key == "" || value == "" {
				continue
			}
			link[key] = value
			if strings.TrimSpace(state.IDs[key]) != value {
				needsSync = true
			}
		}
		if !needsSync || len(link) == 0 {
			continue
		}
		if out == nil {
			out = map[string]map[string]string{}
		}
		out[psp] = link
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
func matchPrice(current []billingservice.CatalogPrice, price Price, accessDurationHours, trialHours *int, trialAmount *int64, claimed map[uuid.UUID]struct{}) *billingservice.CatalogPrice {
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

func productID(p *billingservice.CatalogProduct) uuid.UUID {
	if p == nil {
		return uuid.Nil
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

func creditsSpec(credits []CreditGrant) billingservice.CreditsSpec {
	if len(credits) == 0 {
		return nil
	}
	out := make(billingservice.CreditsSpec, len(credits))
	for _, credit := range credits {
		if credit.Amount == nil {
			continue
		}
		out[credit.Key] = billingservice.CreditGrantSpec{
			Unit:        strings.TrimSpace(credit.Unit),
			Amount:      *credit.Amount,
			ExpiryHours: credit.ExpiryHours,
			Cadence:     billingservice.CreditGrantCadence(strings.TrimSpace(credit.Cadence)),
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// HasChanges reports whether the plan would mutate anything.
func (plan *ApplyPlan) HasChanges() bool {
	for gi := range plan.Groups {
		gp := &plan.Groups[gi]
		if len(gp.RemovedProducts) > 0 {
			return true
		}
		for pi := range gp.Products {
			pp := &gp.Products[pi]
			if pp.Action != ProductUnchanged {
				return true
			}
			for _, price := range pp.Prices {
				if price.Action != PriceUnchanged {
					return true
				}
			}
		}
	}
	return false
}

// String renders the plan as a terraform-style change log.
func (plan *ApplyPlan) String() string {
	var b strings.Builder
	for gi := range plan.Groups {
		gp := &plan.Groups[gi]
		fmt.Fprintf(&b, "tier_group %s\n", gp.Key)
		for pi := range gp.Products {
			pp := &gp.Products[pi]
			fmt.Fprintf(&b, "  %s product %s\n", symbol(string(pp.Action)), pp.Key)
			var changes []string
			for i := range pp.Prices {
				plp := &pp.Prices[i]
				changes = append(changes, fmt.Sprintf("%s %s", plp.Action, plp.Label))
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
func (plan *ApplyPlan) Print(out io.Writer, dryRun bool) {
	if dryRun {
		fmt.Fprintln(out, "catalog plan (dry run; no changes applied):")
	} else {
		fmt.Fprintln(out, "catalog plan:")
	}
	fmt.Fprint(out, plan.String())
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
