package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/modules/catalog"
)

// Issue #357 — provider-side catalog extras: detection + (--exhaustive) archive.
//
// DEFAULT (`bootstrap apply`): DetectCatalogExtras enumerates every
// provider-side catalog object (Stripe products + prices, NMI recurring plans)
// and reports those NOT present in the local catalog. Detection is READ-ONLY —
// it works in every operating mode — and extras are ignorable: the default
// path never touches them.
//
// --exhaustive (`bootstrap apply --exhaustive`): the local catalog is treated
// as authoritative-and-exhaustive — ArchiveCatalogExtras archives (NEVER
// deletes) extras that bear OpenRails ownership markers. Existing
// subscriptions keep billing; only NEW purchases of archived objects become
// impossible. Per-provider archive semantics:
//
//   - Stripe: products/prices -> active=false (Stripe's documented archive:
//     existing subscriptions continue, the object can no longer be purchased).
//   - NMI: LOG-ONLY + pending manual action. NMI has no plan-archive
//     primitive, and plan deletion is both irreversible and documented-unsafe
//     while customers use the plan ("Once a plan is deleted, this action
//     cannot be undone. Please ensure no customers are using the plan before
//     proceeding") — and plan changes propagate live to subscribers ("This
//     plan is used by X customer(s). All customers using this plan will be
//     affected by your changes."), so OpenRails cannot guarantee existing
//     subscriptions keep billing after a delete. The operator confirms zero
//     subscribers in the NMI control center, then deletes the plan manually.
//     (Source: support.nmi.com "Recurring via the Virtual Terminal: Plans and
//     Subscriptions".)
//   - CCBill: no catalog API at all (FlexForms are write-only) -> note only;
//     archive is a pending manual action in the CCBill admin portal.
//   - Solana: the official Subscriptions program DOES have an archive path —
//     updatePlan (discriminator 8) can set planStatus=sunset, which blocks new
//     subscribe calls (program error 500 "Plan is in sunset status") while
//     existing subscriptions continue — but plans are PDAs at derived
//     addresses with no enumeration API, so account-level extras are not
//     observable; any plan OpenRails can see is by construction in the local
//     catalog. Note only. (BuildUpdatePlan is also not yet wired in
//     internal/integrations/solana/subscriptions.)
//
// OWNERSHIP-MARKER GUARD: only objects bearing OUR markers are ever archived —
// Stripe objects must carry the openrails_product_key / openrails_price_key
// metadata or an "openrails."-prefixed lookup_key; NMI plans must match the
// content-addressed "<slug>-<currency>-<amount>-<cycle>" plan_id shape.
// Foreign (tenant-owned, unrelated) provider objects are LISTED in the report
// but NEVER touched, even under --exhaustive.
//
// MODE INTERPLAY: archive is a provider write — under mode=limited/readonly it
// refuses UP FRONT with ErrCatalogExtrasArchiveDisabled (the readonly wire
// chokes would block the POSTs anyway, but we fail loudly before doing
// anything rather than half-run). Detection always works.

// CatalogExtra is one provider-side catalog object that is not in the local
// catalog. Owned reports whether it bears an OpenRails ownership marker (the
// archive guard); Active is the provider-side active flag (NMI plans have no
// such flag and always report true).
type CatalogExtra struct {
	Provider   string `json:"provider"`    // "stripe" | "mobius"
	ObjectType string `json:"object_type"` // "product" | "price" | "plan"
	ExternalID string `json:"external_id"`
	Label      string `json:"label,omitempty"` // name / lookup_key / plan name
	Owned      bool   `json:"owned"`
	Active     bool   `json:"active"`
}

// CatalogExtrasNote explains why a provider could not be (fully) scanned, or
// states a structural per-provider caveat (CCBill / Solana).
type CatalogExtrasNote struct {
	Provider string `json:"provider"`
	Note     string `json:"note"`
}

// CatalogExtrasReport is the result of one extras-detection pass.
type CatalogExtrasReport struct {
	ScannedStripeProducts int                 `json:"scanned_stripe_products"`
	ScannedStripePrices   int                 `json:"scanned_stripe_prices"`
	ScannedNMIPlans       int                 `json:"scanned_nmi_plans"`
	Extras                []CatalogExtra      `json:"extras"`
	Notes                 []CatalogExtrasNote `json:"notes,omitempty"`
}

// ErrCatalogExtrasArchiveDisabled is the up-front refusal for
// ArchiveCatalogExtras under mode=limited/readonly: catalog provider writes
// are gated by the operating mode, and the archive must not half-run.
var ErrCatalogExtrasArchiveDisabled = errors.New(
	"catalog extras archive refused: provider writes are disabled (mode=limited/readonly); extras were NOT archived — re-run under mode=full (extras detection/logging stays available in every mode)")

// CatalogExtraArchiveAction is the per-extra outcome of an archive pass.
type CatalogExtraArchiveAction string

const (
	// CatalogExtraArchived: the provider object was archived (Stripe active=false).
	CatalogExtraArchived CatalogExtraArchiveAction = "archived"
	// CatalogExtraSkippedForeign: no OpenRails ownership marker — never touched.
	CatalogExtraSkippedForeign CatalogExtraArchiveAction = "skipped_foreign"
	// CatalogExtraSkippedInactive: already archived on the provider side.
	CatalogExtraSkippedInactive CatalogExtraArchiveAction = "skipped_already_inactive"
	// CatalogExtraManualActionRequired: this provider has no safe archive API
	// (NMI plan delete is unsafe/irreversible; CCBill has no API) — the Detail
	// explains the manual step.
	CatalogExtraManualActionRequired CatalogExtraArchiveAction = "manual_action_required"
	// CatalogExtraArchiveFailed: the provider write errored; Detail carries it.
	CatalogExtraArchiveFailed CatalogExtraArchiveAction = "failed"
)

// CatalogExtraArchiveOutcome pairs one extra with what the archive pass did.
type CatalogExtraArchiveOutcome struct {
	Extra  CatalogExtra              `json:"extra"`
	Action CatalogExtraArchiveAction `json:"action"`
	Detail string                    `json:"detail,omitempty"`
}

// nmiPlanArchiveManualDetail is the manual-action text attached to owned NMI
// plan extras under --exhaustive. See the file header for the doc citations.
const nmiPlanArchiveManualDetail = "NMI has no plan-archive primitive and plan deletion is irreversible and unsafe while customers use the plan " +
	"(NMI: \"Once a plan is deleted, this action cannot be undone. Please ensure no customers are using the plan before proceeding\"); " +
	"verify the plan has zero subscribers in the NMI control center, then delete it manually"

// ---------------------------------------------------------------------------
// Detection (read-only; every mode)
// ---------------------------------------------------------------------------

// DetectCatalogExtras enumerates the provider-side catalogs (Stripe via the
// stripeapi choke client, NMI via the Query API) and returns the objects that
// are NOT in the local catalog. Read-only; never mutates anything. Providers
// without a read API (CCBill) or without enumeration (Solana) contribute notes.
func (s *Service) DetectCatalogExtras(ctx context.Context) (*CatalogExtrasReport, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	var stripeLister stripeProductLister
	if stripeProc := cfg.GetStripeProcessor(); stripeProc != nil && strings.TrimSpace(stripeProc.SecretKey) != "" {
		stripeLister = &catalog.StripeCatalogService{Config: cfg}
	}
	var nmiLister nmiPlanLister
	if s.rt != nil && s.rt.NMIClients != nil {
		if client, ok := s.rt.NMIClients["mobius"]; ok && client != nil {
			nmiLister = client
		}
	}
	return s.detectCatalogExtrasWith(ctx, stripeLister, nmiLister)
}

// detectCatalogExtrasWith is the testable core: the listers are injected so
// unit tests can supply fixture data. A nil lister skips that provider's pass
// (with a note).
func (s *Service) detectCatalogExtrasWith(ctx context.Context, stripeLister stripeProductLister, nmiLister nmiPlanLister) (*CatalogExtrasReport, error) {
	snap, err := s.buildLocalCatalogSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	report := &CatalogExtrasReport{}

	if stripeLister != nil {
		products, prices, ferr := fetchStripeCatalog(ctx, stripeLister)
		if ferr != nil {
			return nil, ferr
		}
		report.ScannedStripeProducts = len(products)
		report.ScannedStripePrices = len(prices)
		report.Extras = append(report.Extras, computeStripeExtras(products, prices, snap)...)
	} else {
		report.Notes = append(report.Notes, CatalogExtrasNote{
			Provider: "stripe",
			Note:     "not configured; Stripe extras not scanned",
		})
	}

	if nmiLister != nil {
		plans, ferr := fetchNMIPlans(nmiLister)
		if ferr != nil {
			return nil, ferr
		}
		report.ScannedNMIPlans = len(plans)
		report.Extras = append(report.Extras, computeNMIExtras(plans, snap)...)
	} else {
		report.Notes = append(report.Notes, CatalogExtrasNote{
			Provider: "mobius",
			Note:     "not configured; NMI recurring-plan extras not scanned",
		})
	}

	// Structural per-provider caveats (always present, regardless of config).
	report.Notes = append(report.Notes,
		CatalogExtrasNote{
			Provider: "ccbill",
			Note:     "no catalog read API (FlexForms are write-only): extras cannot be enumerated; review/retire FlexForms manually in the CCBill admin portal",
		},
		CatalogExtrasNote{
			Provider: "solana",
			Note:     "the Subscriptions program has no plan-enumeration API (plans are PDAs at derived addresses), so account extras are not observable; plan sunsetting (updatePlan status=sunset) exists on-chain but is not wired",
		},
	)
	return report, nil
}

// computeStripeExtras is the pure Stripe diff: remote products/prices that the
// local catalog neither links by id nor matches by content key. Owned = the
// object bears an OpenRails marker (openrails_product_key / openrails_price_key
// metadata, or an "openrails."-prefixed lookup_key).
func computeStripeExtras(products []catalog.StripeProduct, prices []catalog.StripePrice, snap localCatalogSnapshot) []CatalogExtra {
	var out []CatalogExtra
	for _, sp := range products {
		if _, linked := snap.stripeProductIDs[sp.ID]; linked {
			continue // a local price stores this stripe product id
		}
		productKey := strings.TrimSpace(sp.Metadata[catalog.StripeMetadataOpenRailsProductKey])
		if productKey != "" {
			if _, ok := snap.productBySlug[productKey]; ok {
				continue // content-matched to a local product
			}
		}
		out = append(out, CatalogExtra{
			Provider:   "stripe",
			ObjectType: "product",
			ExternalID: sp.ID,
			Label:      strings.TrimSpace(sp.Name),
			Owned:      productKey != "",
			Active:     sp.Active,
		})
	}
	for _, sp := range prices {
		if _, linked := snap.stripePriceIDs[sp.ID]; linked {
			continue // a local price stores this stripe price id
		}
		contentKey := stripePriceContentKey(sp)
		if contentKey != "" {
			if _, ok := snap.priceByContentKey[contentKey]; ok {
				continue // content-matched to a local price
			}
		}
		label := strings.TrimSpace(sp.LookupKey)
		if label == "" {
			label = strings.TrimSpace(sp.Nickname)
		}
		out = append(out, CatalogExtra{
			Provider:   "stripe",
			ObjectType: "price",
			ExternalID: sp.ID,
			Label:      label,
			Owned:      contentKey != "",
			Active:     sp.Active,
		})
	}
	return out
}

// computeNMIExtras is the pure NMI diff: recurring plans on the account whose
// plan_id is not referenced by any local price. Owned = the plan_id matches the
// content-addressed "<slug>-<currency>-<amount>-<cycle>" shape OpenRails mints
// (mobiusDeterministicPlanID). NMI plans have no active flag, so Active is
// always true.
func computeNMIExtras(plans []nmiPlan, snap localCatalogSnapshot) []CatalogExtra {
	known := make(map[string]struct{}, len(snap.nmiPlanIDByOpenRailsPrice))
	for _, planID := range snap.nmiPlanIDByOpenRailsPrice {
		if planID != "" {
			known[planID] = struct{}{}
		}
	}
	var out []CatalogExtra
	for _, p := range plans {
		if p.PlanID == "" {
			continue
		}
		if _, ok := known[p.PlanID]; ok {
			continue
		}
		out = append(out, CatalogExtra{
			Provider:   "mobius",
			ObjectType: "plan",
			ExternalID: p.PlanID,
			Label:      strings.TrimSpace(p.PlanName),
			Owned:      isContentAddressedNMIPlanID(p.PlanID),
			Active:     true,
		})
	}
	return out
}

// isContentAddressedNMIPlanID reports whether a plan_id matches the
// content-addressed shape OpenRails mints: "<slug>-<currency>-<amount>-<cycle>"
// where currency is a 3-letter code, amount is an integer, and cycle is a day
// count or "onetime" (see mobiusDeterministicPlanID). The slug may itself
// contain hyphens, so the id is parsed from the right. Operator-chosen plan ids
// that happen not to match this shape are treated as foreign (never archived).
func isContentAddressedNMIPlanID(planID string) bool {
	parts := strings.Split(strings.TrimSpace(planID), "-")
	if len(parts) < 4 {
		return false
	}
	cycle := parts[len(parts)-1]
	amount := parts[len(parts)-2]
	currency := parts[len(parts)-3]
	slug := strings.Join(parts[:len(parts)-3], "-")
	if slug == "" {
		return false
	}
	if cycle != "onetime" && !isAllDigits(cycle) {
		return false
	}
	if !isAllDigits(amount) {
		return false
	}
	if len(currency) != 3 || !isAllLowerAlpha(currency) {
		return false
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isAllLowerAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Archive (--exhaustive; provider WRITE — mode-gated)
// ---------------------------------------------------------------------------

// stripeCatalogArchiver is the write subset of StripeCatalogService the archive
// pass needs. An interface so unit tests can capture the exact archive calls
// without a live Stripe account.
type stripeCatalogArchiver interface {
	UpdateProduct(ctx context.Context, stripeProductID string, params catalog.UpdateProductParams) error
	UpdatePrice(ctx context.Context, stripePriceID string, params catalog.UpdatePriceParams) error
}

// ArchiveCatalogExtras archives (never deletes) the OWNED extras from a
// detection pass. Foreign extras are skipped untouched; NMI extras surface as
// pending manual actions (see nmiPlanArchiveManualDetail). Refuses up front
// under mode=limited/readonly with ErrCatalogExtrasArchiveDisabled.
//
// Returns one outcome per input extra plus an aggregate error when any
// provider write failed (the pass continues past individual failures so a
// partial Stripe outage doesn't hide the remaining outcomes).
func (s *Service) ArchiveCatalogExtras(ctx context.Context, extras []CatalogExtra) ([]CatalogExtraArchiveOutcome, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	if cfg.IsLimitedMode() {
		return nil, ErrCatalogExtrasArchiveDisabled
	}
	var archiver stripeCatalogArchiver
	if stripeProc := cfg.GetStripeProcessor(); stripeProc != nil && strings.TrimSpace(stripeProc.SecretKey) != "" {
		archiver = &catalog.StripeCatalogService{Config: cfg}
	}
	return archiveCatalogExtrasWith(ctx, archiver, extras)
}

// archiveCatalogExtrasWith is the testable core of ArchiveCatalogExtras: the
// Stripe archiver is injected. It assumes the mode gate has already passed.
func archiveCatalogExtrasWith(ctx context.Context, archiver stripeCatalogArchiver, extras []CatalogExtra) ([]CatalogExtraArchiveOutcome, error) {
	outcomes := make([]CatalogExtraArchiveOutcome, 0, len(extras))
	var failed int
	for _, e := range extras {
		switch {
		case !e.Owned:
			// HARD GUARD: no OpenRails ownership marker — never touched, even
			// under --exhaustive.
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra:  e,
				Action: CatalogExtraSkippedForeign,
				Detail: "no OpenRails ownership marker — never touched",
			})
		case e.Provider == "stripe":
			if !e.Active {
				outcomes = append(outcomes, CatalogExtraArchiveOutcome{
					Extra:  e,
					Action: CatalogExtraSkippedInactive,
					Detail: "already inactive on Stripe",
				})
				continue
			}
			if archiver == nil {
				outcomes = append(outcomes, CatalogExtraArchiveOutcome{
					Extra:  e,
					Action: CatalogExtraArchiveFailed,
					Detail: "stripe is not configured",
				})
				failed++
				continue
			}
			inactive := false
			var aerr error
			switch e.ObjectType {
			case "product":
				aerr = archiver.UpdateProduct(ctx, e.ExternalID, catalog.UpdateProductParams{Active: &inactive})
			case "price":
				aerr = archiver.UpdatePrice(ctx, e.ExternalID, catalog.UpdatePriceParams{Active: &inactive})
			default:
				aerr = fmt.Errorf("unknown stripe object type %q", e.ObjectType)
			}
			if aerr != nil {
				outcomes = append(outcomes, CatalogExtraArchiveOutcome{
					Extra:  e,
					Action: CatalogExtraArchiveFailed,
					Detail: aerr.Error(),
				})
				failed++
				continue
			}
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra:  e,
				Action: CatalogExtraArchived,
				Detail: "stripe active=false",
			})
		case e.Provider == "mobius":
			// LOG-ONLY by design: see the file header for the verified NMI
			// semantics. No NMI write is ever issued from this path.
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra:  e,
				Action: CatalogExtraManualActionRequired,
				Detail: nmiPlanArchiveManualDetail,
			})
		default:
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra:  e,
				Action: CatalogExtraManualActionRequired,
				Detail: "provider has no archive API",
			})
		}
	}
	if failed > 0 {
		return outcomes, fmt.Errorf("catalog extras archive: %d of %d archive write(s) failed (see outcomes)", failed, len(extras))
	}
	return outcomes, nil
}
