package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Issue #357 — provider-side catalog extras: detection + (--prune) archive.
// Issue #358 phase D — every archive WRITE flows through the provider intent
// ledger instead of being a direct provider call.
//
// DEFAULT (`push-catalog`): DetectCatalogExtras enumerates every
// provider-side catalog object (Stripe products + prices, NMI recurring plans)
// and reports those NOT present in the local catalog. For Solana — whose plans
// are PDAs with no enumeration API, so FOREIGN plans are unobservable — the
// scan instead derives "sunset-needed" plans from the LOCAL handles: an
// on-chain plan referenced only by non-purchasable (archived/draft) local
// prices that still has status=active should not accept new subscribers.
// Detection is READ-ONLY — it works in every operating mode — and extras are
// ignorable: the default path never touches them.
//
// --prune (`push-catalog --prune`): the local catalog is treated
// as complete — ArchiveCatalogExtras archives (NEVER
// deletes) extras that bear OpenRails ownership markers, by enqueuing
// admin-origin intents on the provider intent ledger (#358) and executing them
// synchronously. Per-provider archive semantics:
//
//   - Stripe: products/prices -> active=false (Stripe's documented archive:
//     existing subscriptions continue, the object can no longer be purchased).
//     Intent types stripe_archive_product / stripe_archive_price.
//   - NMI: LOG-ONLY + pending manual action — NO intent, NO write path. NMI
//     has no plan-archive primitive, and plan deletion is both irreversible
//     and documented-unsafe while customers use the plan ("Once a plan is
//     deleted, this action cannot be undone. Please ensure no customers are
//     using the plan before proceeding") — and plan changes propagate live to
//     subscribers, so OpenRails cannot guarantee existing subscriptions keep
//     billing after a delete. The operator confirms zero subscribers in the
//     NMI control center, then deletes the plan manually. (Source:
//     support.nmi.com "Recurring via the Virtual Terminal: Plans and
//     Subscriptions".)
//   - CCBill: no catalog API at all (FlexForms are write-only) -> note only;
//     archive is a pending manual action in the CCBill admin portal.
//   - Solana: updatePlan status=sunset — the program's exact archive
//     semantics: new subscribe calls are rejected (program error "Plan is in
//     sunset status") while existing subscriptions continue. Intent type
//     solana_sunset_plan.
//
// OWNERSHIP-MARKER GUARD: only objects bearing OUR markers are ever archived —
// Stripe objects must carry the openrails_product_key / openrails_price_key
// metadata or an "openrails."-prefixed lookup_key; NMI plans must match the
// content-addressed "<slug>-<currency>-<amount>-<cycle>" plan_id shape; Solana
// sunset candidates come from our own stored plan handles. Foreign (merchant-
// owned, unrelated) provider objects are LISTED in the report but NEVER
// touched, even under --prune.
//
// MODE INTERPLAY (#358): the intents are admin-origin (the --prune flag
// is an explicit human request), so they EXECUTE under mode=limited and PARK
// under mode=readonly — parked is NOT an error, the scheduled executor drains
// the queue when the mode lifts. A provider being down no longer aborts the
// sweep either: those items retry durably on the ledger.

// CatalogExtra is one provider-side catalog object that is not in the local
// catalog (for Solana: a sunset-needed plan, see the file header). Owned
// reports whether it bears an OpenRails ownership marker (the archive guard);
// Active is the provider-side active flag (NMI plans have no such flag and
// always report true). MarkerKey is the Stripe ownership marker value (a
// product slug / price content key) — the archive intents' relevance re-checks
// it against the live local catalog.
type CatalogExtra struct {
	Provider   string `json:"provider"`    // "stripe" | "mobius" | "solana"
	ObjectType string `json:"object_type"` // "product" | "price" | "plan"
	ExternalID string `json:"external_id"`
	Label      string `json:"label,omitempty"` // name / lookup_key / plan name / content key
	Owned      bool   `json:"owned"`
	Active     bool   `json:"active"`
	MarkerKey  string `json:"marker_key,omitempty"`
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
	ScannedSolanaPlans    int                 `json:"scanned_solana_plans"`
	Extras                []CatalogExtra      `json:"extras"`
	Notes                 []CatalogExtrasNote `json:"notes,omitempty"`
}

// CatalogExtraArchiveAction is the per-extra outcome of an archive pass.
type CatalogExtraArchiveAction string

const (
	// CatalogExtraArchived: the provider object was archived (Stripe
	// active=false / Solana plan sunset), or verified already archived/absent.
	CatalogExtraArchived CatalogExtraArchiveAction = "archived"
	// CatalogExtraSkippedForeign: no OpenRails ownership marker — never touched.
	CatalogExtraSkippedForeign CatalogExtraArchiveAction = "skipped_foreign"
	// CatalogExtraSkippedInactive: already archived on the provider side.
	CatalogExtraSkippedInactive CatalogExtraArchiveAction = "skipped_already_inactive"
	// CatalogExtraManualActionRequired: this provider has no safe archive API
	// (NMI plan delete is unsafe/irreversible; CCBill has no API) — the Detail
	// explains the manual step.
	CatalogExtraManualActionRequired CatalogExtraArchiveAction = "manual_action_required"
	// CatalogExtraArchiveParked: the archive intent is recorded durably but did
	// not complete inline (mode gate, provider down, ambiguous outcome). NOT an
	// error — the scheduled executor/verifier drains it; Detail says why.
	CatalogExtraArchiveParked CatalogExtraArchiveAction = "parked"
	// CatalogExtraArchiveSuperseded: between detection and execution the object
	// stopped being an extra (it joined the local catalog) — nothing was done.
	CatalogExtraArchiveSuperseded CatalogExtraArchiveAction = "superseded"
	// CatalogExtraArchiveFailed: the archive failed terminally (or could not be
	// enqueued); Detail carries the reason.
	CatalogExtraArchiveFailed CatalogExtraArchiveAction = "failed"
)

// CatalogExtraArchiveOutcome pairs one extra with what the archive pass did.
type CatalogExtraArchiveOutcome struct {
	Extra  CatalogExtra              `json:"extra"`
	Action CatalogExtraArchiveAction `json:"action"`
	Detail string                    `json:"detail,omitempty"`
	// IntentID references the provider-intent ledger row (when one was
	// enqueued), for reconcile/forensics.
	IntentID string `json:"intent_id,omitempty"`
}

// nmiPlanArchiveManualDetail is the manual-action text attached to owned NMI
// plan extras under --prune. See the file header for the doc citations.
const nmiPlanArchiveManualDetail = "NMI has no plan-archive primitive and plan deletion is irreversible and unsafe while customers use the plan " +
	"(NMI: \"Once a plan is deleted, this action cannot be undone. Please ensure no customers are using the plan before proceeding\"); " +
	"verify the plan has zero subscribers in the NMI control center, then delete it manually"

// ---------------------------------------------------------------------------
// Detection (read-only; every mode)
// ---------------------------------------------------------------------------

// solanaPlanReader is the chain-read surface the Solana sunset-needed scan
// uses (satisfied by *solana.RPCClient; interface for unit tests).
type solanaPlanReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// DetectCatalogExtras enumerates the provider-side catalogs (Stripe via the
// stripeapi choke client, NMI via the Query API, Solana via per-stored-handle
// account reads) and returns the objects that are NOT in the local catalog
// (resp. sunset-needed, for Solana). Read-only; never mutates anything.
// Providers without a read API (CCBill) contribute notes.
func (s *Service) DetectCatalogExtras(ctx context.Context) (*CatalogExtrasReport, error) {
	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	var stripeLister stripeProductLister
	if s.rt != nil {
		if stripeProc := s.rt.Rails.GetStripeRail(); stripeProc != nil && strings.TrimSpace(stripeProc.SecretKey) != "" {
			stripeLister = &catalog.StripeCatalogService{Config: cfg, Rails: s.rt.Rails}
		}
	}
	var nmiLister nmiPlanLister
	if s.rt != nil && s.rt.NMIClients != nil {
		if client, ok := s.rt.NMIClients["mobius"]; ok && client != nil {
			nmiLister = client
		}
	}
	var solanaReader solanaPlanReader
	if s.rt != nil && s.rt.SolanaRPC != nil {
		solanaReader = s.rt.SolanaRPC
	}
	return s.detectCatalogExtrasWith(ctx, stripeLister, nmiLister, solanaReader)
}

// detectCatalogExtrasWith is the testable core: the listers/readers are
// injected so unit tests can supply fixture data. A nil lister skips that
// provider's pass (with a note).
func (s *Service) detectCatalogExtrasWith(ctx context.Context, stripeLister stripeProductLister, nmiLister nmiPlanLister, solanaReader solanaPlanReader) (*CatalogExtrasReport, error) {
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

	if solanaReader != nil {
		solExtras, scanned, solNotes := computeSolanaSunsetExtras(ctx, solanaReader, snap)
		report.ScannedSolanaPlans = scanned
		report.Extras = append(report.Extras, solExtras...)
		report.Notes = append(report.Notes, solNotes...)
	} else {
		report.Notes = append(report.Notes, CatalogExtrasNote{
			Provider: "solana",
			Note:     "rpc not configured; sunset-needed on-chain plans not scanned",
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
			Note:     "the Subscriptions program has no plan-enumeration API (plans are PDAs at derived addresses), so FOREIGN account extras are unobservable; only plans referenced by local handles are scanned for sunset",
		},
	)
	return report, nil
}

// computeStripeExtras is the pure Stripe diff: remote products/prices that the
// local catalog neither links by id nor matches by content key. The extra-ness
// predicate is catalog.ExtrasIndex — the SAME definition the archive intents'
// relevance checks use (#358), so detection and the ledger can never disagree.
// Owned = the object bears an OpenRails marker (openrails_product_key /
// openrails_price_key metadata, or an "openrails."-prefixed lookup_key).
func computeStripeExtras(products []catalog.StripeProduct, prices []catalog.StripePrice, snap localCatalogSnapshot) []CatalogExtra {
	ix := snap.extrasIndex()
	var out []CatalogExtra
	for _, sp := range products {
		extra, productKey := ix.StripeProductExtra(sp)
		if !extra {
			continue
		}
		out = append(out, CatalogExtra{
			Provider:   "stripe",
			ObjectType: "product",
			ExternalID: sp.ID,
			Label:      strings.TrimSpace(sp.Name),
			Owned:      productKey != "",
			Active:     sp.Active,
			MarkerKey:  productKey,
		})
	}
	for _, sp := range prices {
		extra, contentKey := ix.StripePriceExtra(sp)
		if !extra {
			continue
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
			MarkerKey:  contentKey,
		})
	}
	return out
}

// extrasIndex renders the snapshot as the shared extra-ness index
// (catalog.ExtrasIndex) detection and the #358 relevance checks consult.
func (snap localCatalogSnapshot) extrasIndex() catalog.ExtrasIndex {
	ix := catalog.ExtrasIndex{
		StripeProductIDs: make(map[string]struct{}, len(snap.stripeProductIDs)),
		StripePriceIDs:   make(map[string]struct{}, len(snap.stripePriceIDs)),
		ProductSlugs:     make(map[string]struct{}, len(snap.productBySlug)),
		PriceContentKeys: make(map[string]struct{}, len(snap.priceByContentKey)),
	}
	for id := range snap.stripeProductIDs {
		ix.StripeProductIDs[id] = struct{}{}
	}
	for id := range snap.stripePriceIDs {
		ix.StripePriceIDs[id] = struct{}{}
	}
	for slug := range snap.productBySlug {
		ix.ProductSlugs[slug] = struct{}{}
	}
	for ck := range snap.priceByContentKey {
		ix.PriceContentKeys[ck] = struct{}{}
	}
	return ix
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

// computeSolanaSunsetExtras derives the Solana sunset candidates from the
// LOCAL plan handles (there is no on-chain enumeration): a plan PDA referenced
// ONLY by non-purchasable (archived/draft) local prices, whose on-chain
// account still exists with status=active. Reads only. Already-sunset and
// vanished plans are converged — not reported. Read/decode failures become
// notes (the apply must not fail because one RPC read did).
func computeSolanaSunsetExtras(ctx context.Context, reader solanaPlanReader, snap localCatalogSnapshot) ([]CatalogExtra, int, []CatalogExtrasNote) {
	// pda -> is it referenced by ANY purchasable price; plus a representative
	// label (content key of a referencing price).
	type pdaState struct {
		purchasable bool
		label       string
	}
	byPDA := map[string]*pdaState{}
	for _, pr := range snap.priceByID {
		cfg := pr.Rails[string(models.RailSolana)]
		if cfg == nil {
			continue
		}
		pda := strings.TrimSpace(cfg["plan_pda"])
		if pda == "" {
			continue
		}
		st := byPDA[pda]
		if st == nil {
			st = &pdaState{}
			byPDA[pda] = st
		}
		if pr.IsPurchasable() {
			st.purchasable = true
		}
		if st.label == "" {
			if prod := snap.productByID[pr.ProductID.String()]; prod != nil {
				st.label = openRailsPriceContentKey(prod.Slug, pr.Currency, pr.Amount, pr.BillingCycleDays)
			}
		}
	}

	var (
		out     []CatalogExtra
		notes   []CatalogExtrasNote
		scanned int
	)
	for pda, st := range byPDA {
		if st.purchasable {
			continue // the plan is (still) in the live catalog
		}
		pk, err := solanago.PublicKeyFromBase58(pda)
		if err != nil {
			notes = append(notes, CatalogExtrasNote{Provider: "solana", Note: fmt.Sprintf("stored plan_pda %q is not a valid pubkey; skipped", pda)})
			continue
		}
		scanned++
		data, err := reader.GetAccountData(ctx, pk)
		if err != nil {
			notes = append(notes, CatalogExtrasNote{Provider: "solana", Note: fmt.Sprintf("read plan %s failed (%v); sunset state unknown", pda, err)})
			continue
		}
		if len(data) == 0 {
			continue // plan gone from chain; nothing to sunset
		}
		acct, err := subscriptions.DecodePlanAccount(data)
		if err != nil {
			notes = append(notes, CatalogExtrasNote{Provider: "solana", Note: fmt.Sprintf("plan %s is undecodable (%v); skipped", pda, err)})
			continue
		}
		if acct.Status == subscriptions.PlanStatusSunset {
			continue // already archived on-chain; converged
		}
		out = append(out, CatalogExtra{
			Provider:   "solana",
			ObjectType: "plan",
			ExternalID: pda,
			Label:      st.label,
			Owned:      true, // derived from our own stored handle
			Active:     true,
		})
	}
	return out, scanned, notes
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
// Archive (--prune; provider WRITES — through the intent ledger, #358)
// ---------------------------------------------------------------------------

// intentExecutor is the ledger surface the archive pass drives (interface for
// unit tests; satisfied by *intents.Runner).
type intentExecutor interface {
	EnqueueAndExecute(ctx context.Context, p intents.EnqueueParams) (gen.OpenrailsProviderIntent, error)
}

// ArchiveCatalogExtras archives (never deletes) the OWNED extras from a
// detection pass by enqueuing admin-origin provider intents and executing them
// synchronously through the full gate/classify pipeline (#358). Foreign extras
// are skipped untouched; NMI extras surface as pending manual actions (see
// nmiPlanArchiveManualDetail — NMI has NO archive write path, by design).
//
// There is no up-front mode refusal: admin-origin intents execute under
// mode=limited and PARK under readonly — parked outcomes are durable, not
// errors, and the scheduled executor drains them when the mode lifts. The
// returned error aggregates only TERMINAL failures.
func (s *Service) ArchiveCatalogExtras(ctx context.Context, extras []CatalogExtra) ([]CatalogExtraArchiveOutcome, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return archiveCatalogExtrasVia(ctx, rt.IntentRunner(), tid.UUID(), s.now().UTC(), extras)
}

// archiveCatalogExtrasVia is the testable core of ArchiveCatalogExtras: the
// intent executor is injected. The pass continues past individual failures so
// a partial outage doesn't hide the remaining outcomes.
func archiveCatalogExtrasVia(ctx context.Context, exec intentExecutor, tenantID uuid.UUID, now time.Time, extras []CatalogExtra) ([]CatalogExtraArchiveOutcome, error) {
	outcomes := make([]CatalogExtraArchiveOutcome, 0, len(extras))
	var failed int
	enqueue := func(e CatalogExtra, intentType, provider, idempotencyKey string, payload any) {
		row, err := exec.EnqueueAndExecute(ctx, intents.EnqueueParams{
			MerchantID:     tenantID,
			Provider:       provider,
			IntentType:     intentType,
			Payload:        payload,
			IdempotencyKey: idempotencyKey,
			NextAttemptAt:  now,
			Origin:         intents.OriginAdmin,
			OriginReason:   "push-catalog --prune (#357)",
		})
		if err != nil {
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra: e, Action: CatalogExtraArchiveFailed,
				Detail: "enqueue archive intent: " + err.Error(),
			})
			failed++
			return
		}
		o := archiveOutcomeFromIntent(e, row)
		if o.Action == CatalogExtraArchiveFailed {
			failed++
		}
		outcomes = append(outcomes, o)
	}

	for _, e := range extras {
		switch {
		case !e.Owned:
			// HARD GUARD: no OpenRails ownership marker — never touched, even
			// under --prune.
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
			var intentType string
			switch e.ObjectType {
			case "product":
				intentType = intents.TypeStripeArchiveProduct
			case "price":
				intentType = intents.TypeStripeArchivePrice
			default:
				outcomes = append(outcomes, CatalogExtraArchiveOutcome{
					Extra: e, Action: CatalogExtraArchiveFailed,
					Detail: fmt.Sprintf("unknown stripe object type %q", e.ObjectType),
				})
				failed++
				continue
			}
			enqueue(e, intentType, "stripe",
				intents.StripeArchiveIdempotencyKey(intentType, e.ExternalID),
				intents.StripeArchivePayload{ObjectID: e.ExternalID, MarkerKey: e.MarkerKey, Label: e.Label})
		case e.Provider == "mobius":
			// LOG-ONLY by design: see the file header for the verified NMI
			// semantics. No NMI write — and no NMI intent type — exists.
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra:  e,
				Action: CatalogExtraManualActionRequired,
				Detail: nmiPlanArchiveManualDetail,
			})
		case e.Provider == "solana" && e.ObjectType == "plan":
			enqueue(e, intents.TypeSolanaSunsetPlan, "solana",
				intents.SolanaSunsetIdempotencyKey(e.ExternalID),
				intents.SolanaSunsetPayload{PlanPDA: e.ExternalID, Label: e.Label})
		default:
			outcomes = append(outcomes, CatalogExtraArchiveOutcome{
				Extra:  e,
				Action: CatalogExtraManualActionRequired,
				Detail: "provider has no archive API",
			})
		}
	}
	if failed > 0 {
		return outcomes, fmt.Errorf("catalog extras archive: %d of %d archive intent(s) failed terminally (see outcomes)", failed, len(extras))
	}
	return outcomes, nil
}

// archiveOutcomeFromIntent maps the post-execution intent row onto the CLI
// outcome vocabulary: succeeded -> archived (with evidence), still-live
// states -> parked (durable; the executor/verifier drains them), terminal ->
// failed, superseded -> nothing to do.
func archiveOutcomeFromIntent(e CatalogExtra, row gen.OpenrailsProviderIntent) CatalogExtraArchiveOutcome {
	out := CatalogExtraArchiveOutcome{Extra: e, IntentID: row.ID.String()}
	reason := ""
	if row.LastFailureReason != nil {
		reason = *row.LastFailureReason
	}
	switch row.Status {
	case intents.StatusSucceeded:
		out.Action = CatalogExtraArchived
		out.Detail = archivedEvidenceDetail(e, row.ResultEvidence)
	case intents.StatusFailedTerminal:
		out.Action = CatalogExtraArchiveFailed
		out.Detail = reason
	case intents.StatusSuperseded:
		out.Action = CatalogExtraArchiveSuperseded
		out.Detail = reason
	case intents.StatusExpired:
		out.Action = CatalogExtraArchiveFailed
		out.Detail = "intent expired: " + reason
	default:
		// pending (parked), failed_retryable, unknown_needs_verify, in_flight:
		// all durable — the scheduled executor/verifier finishes the job.
		out.Action = CatalogExtraArchiveParked
		detail := reason
		if detail == "" {
			detail = "queued on the provider intent ledger"
		}
		out.Detail = detail + " — durable; the intent executor completes it automatically"
	}
	return out
}

// archivedEvidenceDetail renders a compact human detail from the handler's
// success evidence.
func archivedEvidenceDetail(e CatalogExtra, evidence []byte) string {
	var ev map[string]any
	_ = json.Unmarshal(evidence, &ev)
	truthy := func(k string) bool { b, _ := ev[k].(bool); return b }
	switch {
	case truthy("verified_absent"):
		return "already absent at provider (verified)"
	case truthy("already_inactive"), truthy("verified_inactive"):
		return "already inactive at provider (verified)"
	case truthy("already_sunset"), truthy("verified_sunset"):
		return "already sunset on-chain (verified)"
	case truthy("sunset"):
		return "on-chain plan sunset (update_plan status=sunset)"
	case e.Provider == "stripe":
		return "stripe active=false"
	default:
		return "archived"
	}
}
