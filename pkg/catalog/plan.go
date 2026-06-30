package catalog

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
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
	if len(product.RateCards) > 0 || product.hasMeteredPrice() {
		tierGroupPtr = nil
	}
	tierRank := product.tierRank()
	desiredStatus := statusFromActive(product.Active)

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
			Status:           toModelStatus(desiredStatus),
		}
		if err := planPrices(ctx, applier, m, product, nil, pp, opts); err != nil {
			return nil, err
		}
		return pp, nil
	}

	pp.UpdateID = existing.ID
	name := product.DisplayName
	desc := strings.TrimSpace(product.Description)
	st := toModelStatus(desiredStatus)
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
		Status:           &st,
	}
	if productUnchanged(existing, product, entitlements, credits, tierGroupPtr, tierRank, desiredStatus) {
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

func productUnchanged(existing *billingservice.CatalogProduct, product Product, entitlements map[string]*int, credits billingservice.CreditsSpec, tierGroup *string, tierRank int, desiredStatus string) bool {
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
	if string(existing.Status) != desiredStatus {
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

	for _, price := range product.Prices {
		if price.Model != "" {
			continue
		}
		desiredStatus := statusFromActive(price.Active)
		accessDurationHours, err := normalizeDuration(price.Duration)
		if err != nil {
			return fmt.Errorf("price %s duration: %w", PriceLabel(product.Key, price), err)
		}
		label := PriceLabel(product.Key, price)

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
			case string(match.Status) == desiredStatus:
				plp.Action = PriceUnchanged
			case desiredStatus == StatusActive:
				plp.Action = PriceActivate
			default: // inactive desired but currently active
				plp.Action = PriceArchive
			}
			pp.Prices = append(pp.Prices, plp)
			continue
		}

		// No financial match -> create.
		createReq := billingservice.CreatePriceRequest{
			// ProductID is filled at apply time once the product exists.
			ProductID:           productID(existing),
			UnitAmount:          price.UnitAmount,
			Currency:            price.Currency,
			AccessDurationHours: accessDurationHours,
			AutoRenew:           price.AutoRenew,
			TrialUnitAmount:     trialAmount,
			TrialDurationHours:  trialHours,
			Providers:           price.Providers,
			ProviderLinks:       price.ProviderLinks,
			Status:              toModelStatus(desiredStatus),
		}
		pp.Prices = append(pp.Prices, PricePlan{
			Label:     label,
			Action:    PriceCreate,
			CreateReq: createReq,
		})
	}

	if opts.ArchiveMissingPrices {
		// Archive any ACTIVE OpenRails price not claimed by a declared price.
		for i := range current {
			c := &current[i]
			if _, ok := claimed[c.ID]; ok {
				continue
			}
			if string(c.Status) != StatusActive {
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

// matchPrice finds an existing OpenRails price with the same financial identity
// as the declared price, preferring an unclaimed active match over an archived
// one. Identity is exactly the unique_prices_product_amount_window key:
// (currency, unit_amount, access_duration_hours, auto_renew, trial_unit_amount,
// trial_duration_hours). Providers are NOT part of identity — the DB constraint
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
		if best == nil || (string(best.Status) != StatusActive && string(c.Status) == StatusActive) {
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

// toModelStatus casts a manifest status string to the OpenRails CatalogStatus
// enum exposed on the service request structs. An empty string is left empty so
// the facade applies its own default (active).
func toModelStatus(s string) models.CatalogStatus {
	return models.CatalogStatus(s)
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
	amount := moneyutil.FormatMicrosDecimal(unitAmount)
	if currency == "" || strings.EqualFold(currency, "usd") {
		return "$" + amount
	}
	return strings.ToUpper(currency) + " " + amount
}
