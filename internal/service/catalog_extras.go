package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// Catalog extras are provider-side catalog objects the local catalog lacks.
//
// DetectCatalogExtras is read-only (every mode): it reports Stripe products
// and prices and NMI plans missing locally. Solana plans cannot be enumerated,
// so it instead reports active on-chain plans that only archived local prices
// reference (sunset-needed).
//
// ArchiveCatalogExtras archives, never deletes, owned extras through
// admin-origin provider intents:
//
//   - Stripe: active=false; existing subscriptions continue (intents
//     stripe_archive_product / stripe_archive_price).
//   - Solana: update_plan status=sunset; new subscribes are rejected, existing
//     ones continue (intent solana_sunset_plan).
//   - NMI: manual only. NMI has no plan archive, plan delete is irreversible
//     and unsafe while customers use the plan, and plan changes propagate live
//     to subscribers. The operator confirms zero subscribers, then deletes.
//   - CCBill: no catalog API; manual.
//
// Only objects bearing OpenRails markers are archived: Stripe openrails_*
// metadata or an "openrails." lookup_key, an NMI local-price or financial plan
// id, or a stored Solana plan handle. Foreign objects are reported, never
// touched. Admin-origin intents execute under limited mode and park under
// readonly; parked is durable, not an error.

// CatalogExtra is one provider-side catalog object missing from the local
// catalog (for Solana, a sunset-needed plan). Owned means it bears an
// OpenRails marker (the archive guard). NMI plans have no active flag and
// report Active true. MarkerKey is the Stripe marker value the archive intents
// re-check against the live local catalog.
type CatalogExtra struct {
	Provider   string `json:"provider"`    // "stripe" | "nmi" | "solana"
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

// nmiPlanArchiveManualDetail is the manual-action text for owned NMI plan
// extras.
const nmiPlanArchiveManualDetail = "NMI has no plan-archive primitive and plan deletion is irreversible and unsafe while customers use the plan " +
	"(NMI: \"Once a plan is deleted, this action cannot be undone. Please ensure no customers are using the plan before proceeding\"); " +
	"verify the plan has zero subscribers in the NMI control center, then delete it manually"

// solanaPlanReader is the chain-read surface the Solana sunset-needed scan
// uses (satisfied by *solana.RPCClient; interface for unit tests).
type solanaPlanReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// DetectCatalogExtras returns the provider catalog objects missing from the
// local catalog (for Solana, sunset-needed plans). It never mutates; CCBill,
// with no read API, contributes a note.
func (s *Service) DetectCatalogExtras(ctx context.Context) (*CatalogExtrasReport, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	cfg, err := s.requireConfig()
	if err != nil {
		return nil, err
	}
	var stripeLister catalog.StripeCatalogLister
	if s.rt != nil && s.railArmed(ctx, string(models.RailStripe)) {
		stripeLister = &catalog.StripeCatalogService{StripeClients: s.rt.StripeClients, Config: cfg, Rails: s.rt.RailConfigs}
	}
	var nmiLister catalog.NMIPlanLister
	if client := s.resolveNMIClientForMerchant(ctx); client != nil {
		nmiLister = client
	}
	var solanaReader solanaPlanReader
	if s.rt != nil && s.rt.SolanaRPCResolver != nil && s.railArmed(ctx, string(models.RailSolana)) {
		solanaReader = s.rt.SolanaRPCResolver.ChainReader()
	}
	return s.detectCatalogExtrasWith(ctx, stripeLister, nmiLister, solanaReader)
}

// detectCatalogExtrasWith is the testable core: the listers/readers are
// injected so unit tests can supply fixture data. A nil lister skips that
// provider's pass (with a note).
func (s *Service) detectCatalogExtrasWith(ctx context.Context, stripeLister catalog.StripeCatalogLister, nmiLister catalog.NMIPlanLister, solanaReader solanaPlanReader) (*CatalogExtrasReport, error) {
	productSvc, priceSvc, err := s.requireCatalogServices()
	if err != nil {
		return nil, err
	}
	productRows, err := productSvc.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("load products: %w", err)
	}
	priceRows, err := priceSvc.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("load prices: %w", err)
	}
	snap := catalog.BuildDriftSnapshot(productRows, priceRows, uuid.Nil)
	report := &CatalogExtrasReport{}

	if stripeLister != nil {
		products, prices, ferr := catalog.FetchStripeCatalog(ctx, stripeLister)
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
		remotePlans, ferr := nmiLister.ListRecurringPlans(ctx)
		if ferr != nil {
			return nil, fmt.Errorf("list nmi recurring plans: %w", ferr)
		}
		plans := catalog.MapNMIPlans(remotePlans)
		report.ScannedNMIPlans = len(plans)
		report.Extras = append(report.Extras, computeNMIExtras(plans, snap)...)
	} else {
		report.Notes = append(report.Notes, CatalogExtrasNote{
			Provider: string(models.RailNMI),
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

// computeStripeExtras is the pure Stripe diff. Extra-ness is
// catalog.ExtrasIndex, the same test the archive intents re-check, so
// detection and the ledger never disagree. Owned = the object bears an
// OpenRails metadata marker or an "openrails."-prefixed lookup_key.
func computeStripeExtras(products []catalog.StripeProduct, prices []catalog.StripePrice, snap catalog.DriftSnapshot) []CatalogExtra {
	ix := extrasIndex(snap)
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

// extrasIndex renders the snapshot as the shared catalog.ExtrasIndex.
func extrasIndex(snap catalog.DriftSnapshot) catalog.ExtrasIndex {
	ix := catalog.ExtrasIndex{
		StripeProductIDs: make(map[string]struct{}, len(snap.StripeProductIDs)),
		StripePriceIDs:   make(map[string]struct{}, len(snap.StripePriceIDs)),
		ProductKeys:      make(map[string]struct{}, len(snap.ProductByKey)),
		PriceIDs:         make(map[string]struct{}, len(snap.PriceByID)),
	}
	for id := range snap.StripeProductIDs {
		ix.StripeProductIDs[id] = struct{}{}
	}
	for id := range snap.StripePriceIDs {
		ix.StripePriceIDs[id] = struct{}{}
	}
	for key := range snap.ProductByKey {
		ix.ProductKeys[key] = struct{}{}
	}
	for id := range snap.PriceByID {
		ix.PriceIDs[id] = struct{}{}
	}
	return ix
}

// computeNMIExtras is the pure NMI diff: recurring plans on the account whose
// plan_id is not referenced by any local price. Ownership recognizes current
// local-price IDs and historical OpenRails financial markers. NMI plans have
// no active flag, so Active is always true.
func computeNMIExtras(plans []catalog.NMIPlan, snap catalog.DriftSnapshot) []CatalogExtra {
	known := make(map[string]struct{}, len(snap.NMIPlanByPriceID))
	for _, planID := range snap.NMIPlanByPriceID {
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
			Provider:   string(models.RailNMI),
			ObjectType: "plan",
			ExternalID: p.PlanID,
			Label:      strings.TrimSpace(p.PlanName),
			Owned:      isContentAddressedNMIPlanID(p.PlanID),
			Active:     true,
		})
	}
	return out
}

// computeSolanaSunsetExtras derives sunset candidates from local plan handles
// (no on-chain enumeration): a plan PDA only archived prices reference whose
// on-chain plan is still active. Sunset or vanished plans have converged; read
// and decode failures become notes.
func computeSolanaSunsetExtras(ctx context.Context, reader solanaPlanReader, snap catalog.DriftSnapshot) ([]CatalogExtra, int, []CatalogExtrasNote) {
	// pda -> is it referenced by ANY purchasable price; plus a representative
	// label (content key of a referencing price).
	type pdaState struct {
		purchasable bool
		label       string
	}
	byPDA := map[string]*pdaState{}
	for _, pr := range snap.PriceByID {
		cfg := pr.PSPLinkForRail(models.RailSolana)
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
			if prod := snap.ProductByID[pr.ProductID.String()]; prod != nil {
				st.label = prod.Key + "." + pr.Key + ".v" + strconv.FormatInt(pr.Revision, 10)
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

// isContentAddressedNMIPlanID recognizes local-price plan ids ("or-<hex>", see
// nmiDeterministicPlanID) and the historical financial shape
// "<product-key>-<currency>-<amount>-<cycle>" (cycle: day count or "onetime"),
// parsed from the right since product keys may contain hyphens. Any other plan
// id is foreign and never archived.
func isContentAddressedNMIPlanID(planID string) bool {
	if raw, ok := strings.CutPrefix(strings.TrimSpace(planID), "or-"); ok {
		id, err := uuid.Parse(raw)
		return err == nil && id != uuid.Nil && len(raw) == 32
	}
	parts := strings.Split(strings.TrimSpace(planID), "-")
	if len(parts) < 4 {
		return false
	}
	cycle := parts[len(parts)-1]
	amount := parts[len(parts)-2]
	currency := parts[len(parts)-3]
	productKey := strings.Join(parts[:len(parts)-3], "-")
	if productKey == "" {
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

// intentExecutor is the ledger surface the archive pass drives (interface for
// unit tests; satisfied by *intents.Runner).
type intentExecutor interface {
	EnqueueAndExecute(ctx context.Context, p intents.EnqueueParams) (gen.BillingProviderIntent, error)
}

// ArchiveCatalogExtras archives (never deletes) the owned extras by enqueuing
// admin-origin provider intents and executing them synchronously. Foreign
// extras are skipped; NMI extras become manual actions. Parked intents are
// durable, not errors; the returned error aggregates only terminal failures.
func (s *Service) ArchiveCatalogExtras(ctx context.Context, extras []CatalogExtra) ([]CatalogExtraArchiveOutcome, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

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
			OriginReason:   "push-merchant-catalog --prune (#357)",
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
			// No OpenRails ownership marker: never touched.
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
		case e.Provider == string(models.RailNMI):
			// Manual by design (see the file header): no NMI archive intent exists.
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

// archiveOutcomeFromIntent maps the executed intent row to an outcome:
// succeeded -> archived, live states -> parked (the executor finishes them),
// terminal -> failed, superseded -> nothing to do.
func archiveOutcomeFromIntent(e CatalogExtra, row gen.BillingProviderIntent) CatalogExtraArchiveOutcome {
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
