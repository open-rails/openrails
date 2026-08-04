package service

import (
	"context"
	"github.com/open-rails/openrails/internal/railresolve"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// -- fixtures -----------------------------------------------------------------

// extrasTestSnapshot builds a local catalog with one product ("premium") and one
// price (usd 23_000_000 micros / 30d) linked to stripe price/product ids and an NMI plan id.
func extrasTestSnapshot() localCatalogSnapshot {
	productID := uuid.New()
	priceID := uuid.New()
	cycleHours := 30 * 24
	product := &models.Product{ID: productID, Key: "premium"}
	price := &models.Price{
		ID:                  priceID,
		ProductID:           productID,
		Amount:              23_000_000,
		Currency:            "USD",
		AccessDurationHours: &cycleHours,
		AutoRenew:           true,
		PSPLinks: map[string]map[string]string{
			"stripe": {
				models.RailKeyRail:            "stripe",
				models.RailKeyStripePriceID:   "price_local",
				models.RailKeyStripeProductID: "prod_local",
			},
			"mobius": {
				models.RailKeyRail:   string(models.RailNMI),
				models.RailKeyPlanID: "premium-usd-23000000-30",
			},
		},
	}
	return buildSnapshotFromRows([]*models.Product{product}, []*models.Price{price})
}

// -- extras detection: marker classification (ours vs foreign) -----------------

func TestComputeStripeExtras_MarkerClassification(t *testing.T) {
	snap := extrasTestSnapshot()

	products := []catalog.StripeProduct{
		// Matched by content key (openrails_product_key -> local product key): NOT an extra.
		{ID: "prod_matched", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: "premium"}},
		// Matched by stored stripe product id: NOT an extra even without a marker.
		{ID: "prod_local", Active: true},
		// OpenRails-marked but product key unknown locally: OWNED extra.
		{ID: "prod_ours_extra", Name: "Old Tier", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: "retired"}},
		// No marker at all: FOREIGN extra (logged, never archived).
		{ID: "prod_foreign", Name: "Merchant Native", Active: true},
	}
	prices := []catalog.StripePrice{
		// Matched by content key via lookup_key: NOT an extra.
		{ID: "price_matched", Active: true, LookupKey: "openrails.premium.usd.23000000.30"},
		// Matched by stored stripe price id: NOT an extra.
		{ID: "price_local", Active: true},
		// OpenRails-marked (metadata content key) but not in catalog: OWNED extra.
		{ID: "price_ours_extra", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsPriceKey: "retired.usd.9000000.30"}},
		// OpenRails-marked via lookup_key, already inactive: OWNED extra, Active=false.
		{ID: "price_ours_inactive", Active: false, LookupKey: "openrails.retired.usd.5000000.30"},
		// No marker: FOREIGN extra.
		{ID: "price_foreign", Active: true, Nickname: "merchant price"},
	}

	extras := computeStripeExtras(products, prices, snap)

	byID := map[string]CatalogExtra{}
	for _, e := range extras {
		byID[e.ExternalID] = e
	}
	for _, matched := range []string{"prod_matched", "prod_local", "price_matched", "price_local"} {
		if _, ok := byID[matched]; ok {
			t.Errorf("%s is in the local catalog and must not be reported as an extra", matched)
		}
	}
	want := map[string]struct {
		owned  bool
		active bool
		typ    string
	}{
		"prod_ours_extra":     {owned: true, active: true, typ: "product"},
		"prod_foreign":        {owned: false, active: true, typ: "product"},
		"price_ours_extra":    {owned: true, active: true, typ: "price"},
		"price_ours_inactive": {owned: true, active: false, typ: "price"},
		"price_foreign":       {owned: false, active: true, typ: "price"},
	}
	if len(extras) != len(want) {
		t.Fatalf("expected %d extras, got %d: %+v", len(want), len(extras), extras)
	}
	for id, w := range want {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("expected extra %s missing", id)
		}
		if e.Owned != w.owned || e.Active != w.active || e.ObjectType != w.typ || e.Provider != "stripe" {
			t.Errorf("%s: got %+v want owned=%v active=%v type=%s", id, e, w.owned, w.active, w.typ)
		}
	}
}

func TestComputeNMIExtras_MarkerClassification(t *testing.T) {
	snap := extrasTestSnapshot()
	plans := []nmiPlan{
		{PlanID: "premium-usd-23000000-30", PlanName: "Premium"}, // referenced locally: NOT an extra
		{PlanID: "retired-usd-900-30", PlanName: "Retired"},      // content-addressed shape: OWNED extra
		{PlanID: "legacy-vip-plan", PlanName: "Legacy VIP"},      // operator-chosen id: FOREIGN extra
	}
	extras := computeNMIExtras(plans, snap)
	if len(extras) != 2 {
		t.Fatalf("expected 2 extras, got %d: %+v", len(extras), extras)
	}
	byID := map[string]CatalogExtra{}
	for _, e := range extras {
		byID[e.ExternalID] = e
	}
	if e := byID["retired-usd-900-30"]; !e.Owned || e.Provider != "nmi" || e.ObjectType != "plan" || !e.Active {
		t.Errorf("content-addressed plan must be an owned active plan extra, got %+v", e)
	}
	if e := byID["legacy-vip-plan"]; e.Owned {
		t.Errorf("operator-chosen plan id must be foreign, got %+v", e)
	}
}

func TestIsContentAddressedNMIPlanID(t *testing.T) {
	cases := map[string]bool{
		"premium-usd-23000000-30":  true,
		"pro-eur-999-365":          true,
		"vip-gold-usd-999-onetime": true, // hyphenated slug
		"a-b-c-usd-1-7":            true,
		"legacy-vip-plan":          false, // no money terms
		"premium-usd-23x0-30":      false, // amount not digits
		"premium-us-2300-30":       false, // currency not 3 letters
		"premium-USD-2300-30":      false, // currency not lowercase (content keys lower it)
		"-usd-2300-30":             false, // empty slug
		"usd-2300-30":              false, // too few tokens
		"":                         false,
	}
	for id, want := range cases {
		if got := isContentAddressedNMIPlanID(id); got != want {
			t.Errorf("isContentAddressedNMIPlanID(%q) = %v, want %v", id, got, want)
		}
	}
	// The shape must accept exactly what nmiDeterministicPlanID mints.
	cycle := 30
	if minted := nmiDeterministicPlanID("premium", "USD", 2300, &cycle); !isContentAddressedNMIPlanID(minted) {
		t.Errorf("minted plan id %q must classify as content-addressed", minted)
	}
}

// -- NMI listing over httptest --------------------------------------------------

// TestDetectNMIExtras_OverQueryAPI runs the real NMI client against an httptest
// Query API and asserts the listing + classification, and that NO direct-post
// (write) traffic is ever issued by detection.
func TestDetectNMIExtras_OverQueryAPI(t *testing.T) {
	var sawWrite bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			sawWrite = true
			w.Write([]byte(`{}`))
			return
		}
		if r.URL.Path != "/plans" {
			t.Errorf("unexpected v5 GET %q", r.URL.Path)
		}
		w.Write([]byte(`{"plans":[
			{"object":"plan","id":"premium-usd-23000000-30","plan_name":"Premium","plan_amount":"23.00"},
			{"object":"plan","id":"retired-usd-900-30","plan_name":"Retired","plan_amount":"9.00"},
			{"object":"plan","id":"legacy-vip-plan","plan_name":"Legacy VIP","plan_amount":"5.00"}
		],"next_cursor":null,"has_more":false}`))
	}))
	defer server.Close()

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, false)
	if err != nil {
		t.Fatalf("new nmi client: %v", err)
	}
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL

	plans, err := fetchNMIPlans(context.Background(), client)
	if err != nil {
		t.Fatalf("fetch nmi plans: %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("expected 3 plans, got %d", len(plans))
	}
	extras := computeNMIExtras(plans, extrasTestSnapshot())
	if len(extras) != 2 {
		t.Fatalf("expected 2 extras, got %+v", extras)
	}
	if sawWrite {
		t.Fatal("extras detection issued a direct-post (write) request; it must be read-only")
	}
}

// -- archive: through the provider intent ledger (#358 phase D) -------------------

// fakeIntentExecutor records every EnqueueAndExecute call and scripts the
// returned intent row per intent type.
type fakeIntentExecutor struct {
	calls  []intents.EnqueueParams
	script func(p intents.EnqueueParams) gen.OpenrailsRailIntent
	err    error
}

func (f *fakeIntentExecutor) EnqueueAndExecute(_ context.Context, p intents.EnqueueParams) (gen.OpenrailsRailIntent, error) {
	f.calls = append(f.calls, p)
	if f.err != nil {
		return gen.OpenrailsRailIntent{}, f.err
	}
	if f.script != nil {
		return f.script(p), nil
	}
	return gen.OpenrailsRailIntent{
		ID:         uuid.New(),
		IntentType: p.IntentType,
		Rail:       p.Provider,
		Status:     intents.StatusSucceeded,
	}, nil
}

func succeededRow(p intents.EnqueueParams, evidence string) gen.OpenrailsRailIntent {
	return gen.OpenrailsRailIntent{
		ID: uuid.New(), IntentType: p.IntentType, Rail: p.Provider,
		Status: intents.StatusSucceeded, ResultEvidence: []byte(evidence),
	}
}

func TestArchiveCatalogExtrasVia_EnqueuesOnlyOwnedActiveObjects(t *testing.T) {
	exec := &fakeIntentExecutor{script: func(p intents.EnqueueParams) gen.OpenrailsRailIntent {
		return succeededRow(p, `{"archived":true}`)
	}}
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_ours", Owned: true, Active: true, MarkerKey: "retired.usd.9000000.30"},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_ours", Owned: true, Active: true, MarkerKey: "retired"},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_foreign", Owned: false, Active: true},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_foreign", Owned: false, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_ours_inactive", Owned: true, Active: false},
		{Provider: "nmi", ObjectType: "plan", ExternalID: "retired-usd-900-30", Owned: true, Active: true},
		{Provider: "nmi", ObjectType: "plan", ExternalID: "legacy-vip-plan", Owned: false, Active: true},
		{Provider: "solana", ObjectType: "plan", ExternalID: "5tzFkiKscXHK5ZXCGbXZxdw7gTfCvqSGpHGxVJD6oxBd", Owned: true, Active: true},
	}
	outcomes, err := archiveCatalogExtrasVia(context.Background(), exec, dbtest.TestMerchantID.UUID(), time.Now().UTC(), extras)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(outcomes) != len(extras) {
		t.Fatalf("expected %d outcomes, got %d", len(extras), len(outcomes))
	}

	// Exactly the OWNED ACTIVE stripe objects + the solana plan became intents.
	if len(exec.calls) != 3 {
		t.Fatalf("expected exactly 3 archive intents, got %d: %+v", len(exec.calls), exec.calls)
	}
	byType := map[string]intents.EnqueueParams{}
	for _, c := range exec.calls {
		byType[c.IntentType] = c
		if c.Origin != intents.OriginAdmin {
			t.Errorf("%s: archive intents must be ADMIN-origin (the --prune flag is a human request), got %q", c.IntentType, c.Origin)
		}
		if c.MerchantID != dbtest.TestMerchantID.UUID() {
			t.Errorf("%s: merchant not stamped", c.IntentType)
		}
	}
	if c, ok := byType[intents.TypeStripeArchivePrice]; !ok {
		t.Error("missing stripe_archive_price intent")
	} else if c.IdempotencyKey != intents.StripeArchiveIdempotencyKey(intents.TypeStripeArchivePrice, "price_ours") {
		t.Errorf("price idempotency key not content-addressed: %q", c.IdempotencyKey)
	}
	if c, ok := byType[intents.TypeStripeArchiveProduct]; !ok {
		t.Error("missing stripe_archive_product intent")
	} else if p, _ := c.Payload.(intents.StripeArchivePayload); p.ObjectID != "prod_ours" || p.MarkerKey != "retired" {
		t.Errorf("product payload must carry object id + ownership marker, got %+v", p)
	}
	if c, ok := byType[intents.TypeSolanaSunsetPlan]; !ok {
		t.Error("missing solana_sunset_plan intent")
	} else if p, _ := c.Payload.(intents.SolanaSunsetPayload); p.PlanPDA != "5tzFkiKscXHK5ZXCGbXZxdw7gTfCvqSGpHGxVJD6oxBd" {
		t.Errorf("solana payload must carry the plan pda, got %+v", p)
	}

	byID := map[string]CatalogExtraArchiveOutcome{}
	for _, o := range outcomes {
		byID[o.Extra.ExternalID] = o
	}
	for id, wantAction := range map[string]CatalogExtraArchiveAction{
		"price_ours":          CatalogExtraArchived,
		"prod_ours":           CatalogExtraArchived,
		"price_foreign":       CatalogExtraSkippedForeign,
		"prod_foreign":        CatalogExtraSkippedForeign,
		"price_ours_inactive": CatalogExtraSkippedInactive,
		"retired-usd-900-30":  CatalogExtraManualActionRequired,
		"legacy-vip-plan":     CatalogExtraSkippedForeign,
		"5tzFkiKscXHK5ZXCGbXZxdw7gTfCvqSGpHGxVJD6oxBd": CatalogExtraArchived,
	} {
		if got := byID[id].Action; got != wantAction {
			t.Errorf("%s: expected action %s, got %s", id, wantAction, got)
		}
	}
	// The NMI manual action carries the deletion-is-unsafe explanation; NMI
	// never becomes an intent (it has NO archive write path, by design).
	if d := byID["retired-usd-900-30"].Detail; !strings.Contains(d, "zero subscribers") {
		t.Errorf("NMI manual action detail must instruct verifying zero subscribers, got %q", d)
	}
	for _, c := range exec.calls {
		if c.Provider == "nmi" {
			t.Fatalf("an NMI archive intent was enqueued; NMI must stay manual-only: %+v", c)
		}
	}
}

func TestArchiveCatalogExtrasVia_ForeignNeverTouchedEvenOnExhaustive(t *testing.T) {
	exec := &fakeIntentExecutor{}
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_foreign", Owned: false, Active: true},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_foreign", Owned: false, Active: true},
		{Provider: "nmi", ObjectType: "plan", ExternalID: "merchant-plan", Owned: false, Active: true},
	}
	outcomes, err := archiveCatalogExtrasVia(context.Background(), exec, dbtest.TestMerchantID.UUID(), time.Now().UTC(), extras)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("foreign objects must never become archive intents: %+v", exec.calls)
	}
	for _, o := range outcomes {
		if o.Action != CatalogExtraSkippedForeign {
			t.Errorf("%s: expected skipped_foreign, got %s", o.Extra.ExternalID, o.Action)
		}
	}
}

// TestArchiveCatalogExtrasVia_ParkedIsDurableNotError pins ruling #358-D: a
// gate-parked (readonly) or provider-down intent is reported PARKED with the
// reason — the sweep does NOT abort and does NOT error; the scheduled executor
// drains the queue later.
func TestArchiveCatalogExtrasVia_ParkedIsDurableNotError(t *testing.T) {
	reason := "mode=readonly blocks all provider writes"
	exec := &fakeIntentExecutor{script: func(p intents.EnqueueParams) gen.OpenrailsRailIntent {
		return gen.OpenrailsRailIntent{
			ID: uuid.New(), IntentType: p.IntentType, Rail: p.Provider,
			Status: intents.StatusPending, LastFailureReason: &reason,
		}
	}}
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_a", Owned: true, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_b", Owned: true, Active: true},
	}
	outcomes, err := archiveCatalogExtrasVia(context.Background(), exec, dbtest.TestMerchantID.UUID(), time.Now().UTC(), extras)
	if err != nil {
		t.Fatalf("parked outcomes are NOT an error, got: %v", err)
	}
	for _, o := range outcomes {
		if o.Action != CatalogExtraArchiveParked {
			t.Errorf("%s: expected parked, got %s", o.Extra.ExternalID, o.Action)
		}
		if !strings.Contains(o.Detail, "mode=readonly") {
			t.Errorf("%s: parked detail must carry the gate reason, got %q", o.Extra.ExternalID, o.Detail)
		}
		if o.IntentID == "" {
			t.Errorf("%s: parked outcome must reference the durable intent row", o.Extra.ExternalID)
		}
	}
}

func TestArchiveCatalogExtrasVia_TerminalFailuresAggregate(t *testing.T) {
	reason := "stripe refused"
	exec := &fakeIntentExecutor{script: func(p intents.EnqueueParams) gen.OpenrailsRailIntent {
		return gen.OpenrailsRailIntent{
			ID: uuid.New(), IntentType: p.IntentType, Rail: p.Provider,
			Status: intents.StatusFailedTerminal, LastFailureReason: &reason,
		}
	}}
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_a", Owned: true, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_b", Owned: true, Active: true},
	}
	outcomes, err := archiveCatalogExtrasVia(context.Background(), exec, dbtest.TestMerchantID.UUID(), time.Now().UTC(), extras)
	if err == nil {
		t.Fatal("expected an aggregate error when archive intents fail terminally")
	}
	if len(outcomes) != 2 {
		t.Fatalf("the pass must continue past failures; got %d outcomes", len(outcomes))
	}
	for _, o := range outcomes {
		if o.Action != CatalogExtraArchiveFailed {
			t.Errorf("%s: expected failed, got %s", o.Extra.ExternalID, o.Action)
		}
	}
}

// -- solana sunset-needed detection ------------------------------------------------

// fakeSolanaReader maps base58 PDA -> raw account bytes (nil entry = absent).
type fakeSolanaReader struct {
	accounts map[string][]byte
	err      error
}

func (f *fakeSolanaReader) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.accounts[address.String()], nil
}

// encodeTestPlanAccount builds a minimal valid Plan account blob with the
// given status.
func encodeTestPlanAccount(status uint8) []byte {
	blob := make([]byte, subscriptions.PlanAccountSize)
	blob[0] = 1 // plan account discriminator
	blob[33] = 1
	blob[34] = status
	return blob
}

func TestComputeSolanaSunsetExtras(t *testing.T) {
	cycleHours := 30 * 24
	productID := uuid.New()
	product := &models.Product{ID: productID, Key: "premium"}
	mkPrice := func(pda string, archived bool) *models.Price {
		return &models.Price{
			ID: uuid.New(), ProductID: productID, Amount: 23_000_000, Currency: "USD",
			AccessDurationHours: &cycleHours, AutoRenew: true, Archived: archived,
			PSPLinks: map[string]map[string]string{
				string(models.RailSolana): {models.RailKeyRail: string(models.RailSolana), "plan_pda": pda},
			},
		}
	}
	pdaActiveArchived := solanago.NewWallet().PublicKey().String() // archived price, plan still active -> EXTRA
	pdaSunsetArchived := solanago.NewWallet().PublicKey().String() // archived price, plan already sunset -> converged
	pdaAbsentArchived := solanago.NewWallet().PublicKey().String() // archived price, plan gone -> nothing to do
	pdaActiveLive := solanago.NewWallet().PublicKey().String()     // ACTIVE price -> in the live catalog, never an extra

	snap := buildSnapshotFromRows([]*models.Product{product}, []*models.Price{
		mkPrice(pdaActiveArchived, true),
		mkPrice(pdaSunsetArchived, true),
		mkPrice(pdaAbsentArchived, true),
		mkPrice(pdaActiveLive, false),
	})
	reader := &fakeSolanaReader{accounts: map[string][]byte{
		pdaActiveArchived: encodeTestPlanAccount(1), // active
		pdaSunsetArchived: encodeTestPlanAccount(0), // sunset
		pdaAbsentArchived: nil,
		pdaActiveLive:     encodeTestPlanAccount(1),
	}}

	extras, scanned, notes := computeSolanaSunsetExtras(context.Background(), reader, snap)
	if len(notes) != 0 {
		t.Fatalf("unexpected notes: %+v", notes)
	}
	if scanned != 3 {
		t.Errorf("expected 3 scanned plans (the live-price plan is never read), got %d", scanned)
	}
	if len(extras) != 1 {
		t.Fatalf("expected exactly 1 sunset-needed extra, got %+v", extras)
	}
	e := extras[0]
	if e.Provider != "solana" || e.ObjectType != "plan" || e.ExternalID != pdaActiveArchived || !e.Owned || !e.Active {
		t.Errorf("unexpected extra %+v", e)
	}
	if e.Label != "premium.usd.23000000.30" {
		t.Errorf("label should be the price content key, got %q", e.Label)
	}
}

// -- live smoke (read-only; opt-in) ------------------------------------------------

// TestLiveStripeExtrasListing runs the extras-detection LISTING (GETs only;
// nothing is ever archived) against a real Stripe account. Opt-in via
// OPENRAILS_LIVE_STRIPE_KEY; use a TEST-mode key.
func TestLiveStripeExtrasListing(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("OPENRAILS_LIVE_STRIPE_KEY"))
	if key == "" {
		t.Skip("set OPENRAILS_LIVE_STRIPE_KEY (a Stripe TEST key) to run the live read-only extras listing")
	}
	rails := railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: key}},
	}
	lister := &catalog.StripeCatalogService{Config: &config.Config{}, Rails: rails}
	products, prices, err := fetchStripeCatalog(context.Background(), lister)
	if err != nil {
		t.Fatalf("live stripe listing: %v", err)
	}
	// Empty local snapshot: every remote object shows up classified ours/foreign.
	extras := computeStripeExtras(products, prices, buildSnapshotFromRows(nil, nil))
	t.Logf("live stripe account: %d products, %d prices, %d extras vs an empty catalog", len(products), len(prices), len(extras))
	for _, e := range extras {
		marker := "foreign"
		if e.Owned {
			marker = "openrails-marked"
		}
		t.Logf("  %s %s (%s) active=%v [%s]", e.ObjectType, e.ExternalID, e.Label, e.Active, marker)
	}
}
