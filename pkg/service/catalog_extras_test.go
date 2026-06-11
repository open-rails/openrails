package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// -- fixtures -----------------------------------------------------------------

// extrasTestSnapshot builds a local catalog with one product ("premium") and one
// price (usd 2300 / 30d) linked to stripe price/product ids and an NMI plan id.
func extrasTestSnapshot() localCatalogSnapshot {
	productID := uuid.New()
	priceID := uuid.New()
	cycle := 30
	product := &models.Product{ID: productID, Slug: "premium"}
	price := &models.Price{
		ID:               priceID,
		ProductID:        productID,
		Amount:           2300,
		Currency:         "usd",
		BillingCycleDays: &cycle,
		Processors: map[string]map[string]string{
			"stripe": {
				models.ProcessorKeyStripePriceID:   "price_local",
				models.ProcessorKeyStripeProductID: "prod_local",
			},
			string(models.ProcessorMobius): {
				models.ProcessorKeyPlanID: "premium-usd-2300-30",
			},
		},
	}
	return buildSnapshotFromRows([]*models.Product{product}, []*models.Price{price})
}

// -- extras detection: marker classification (ours vs foreign) -----------------

func TestComputeStripeExtras_MarkerClassification(t *testing.T) {
	snap := extrasTestSnapshot()

	products := []catalog.StripeProduct{
		// Matched by content key (openrails_product_key -> local slug): NOT an extra.
		{ID: "prod_matched", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: "premium"}},
		// Matched by stored stripe product id: NOT an extra even without a marker.
		{ID: "prod_local", Active: true},
		// OpenRails-marked but slug unknown locally: OWNED extra.
		{ID: "prod_ours_extra", Name: "Old Tier", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsProductKey: "retired"}},
		// No marker at all: FOREIGN extra (logged, never archived).
		{ID: "prod_foreign", Name: "Tenant Native", Active: true},
	}
	prices := []catalog.StripePrice{
		// Matched by content key via lookup_key: NOT an extra.
		{ID: "price_matched", Active: true, LookupKey: "openrails.premium.usd.2300.30"},
		// Matched by stored stripe price id: NOT an extra.
		{ID: "price_local", Active: true},
		// OpenRails-marked (metadata content key) but not in catalog: OWNED extra.
		{ID: "price_ours_extra", Active: true, Metadata: map[string]string{catalog.StripeMetadataOpenRailsPriceKey: "retired.usd.900.30"}},
		// OpenRails-marked via lookup_key, already inactive: OWNED extra, Active=false.
		{ID: "price_ours_inactive", Active: false, LookupKey: "openrails.retired.usd.500.30"},
		// No marker: FOREIGN extra.
		{ID: "price_foreign", Active: true, Nickname: "tenant price"},
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
		{PlanID: "premium-usd-2300-30", PlanName: "Premium"}, // referenced locally: NOT an extra
		{PlanID: "retired-usd-900-30", PlanName: "Retired"},  // content-addressed shape: OWNED extra
		{PlanID: "legacy-vip-plan", PlanName: "Legacy VIP"},  // operator-chosen id: FOREIGN extra
	}
	extras := computeNMIExtras(plans, snap)
	if len(extras) != 2 {
		t.Fatalf("expected 2 extras, got %d: %+v", len(extras), extras)
	}
	byID := map[string]CatalogExtra{}
	for _, e := range extras {
		byID[e.ExternalID] = e
	}
	if e := byID["retired-usd-900-30"]; !e.Owned || e.Provider != "mobius" || e.ObjectType != "plan" || !e.Active {
		t.Errorf("content-addressed plan must be an owned active plan extra, got %+v", e)
	}
	if e := byID["legacy-vip-plan"]; e.Owned {
		t.Errorf("operator-chosen plan id must be foreign, got %+v", e)
	}
}

func TestIsContentAddressedNMIPlanID(t *testing.T) {
	cases := map[string]bool{
		"premium-usd-2300-30":      true,
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
	// The shape must accept exactly what mobiusDeterministicPlanID mints.
	cycle := 30
	if minted := mobiusDeterministicPlanID("premium", "USD", 2300, &cycle); !isContentAddressedNMIPlanID(minted) {
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
		_ = r.ParseForm()
		if r.Form.Get("report_type") != "recurring_plans" {
			sawWrite = true
			w.Write([]byte("response=1"))
			return
		}
		w.Write([]byte(`<nm_response>
			<plan><plan_id>premium-usd-2300-30</plan_id><plan_name>Premium</plan_name><plan_amount>23.00</plan_amount></plan>
			<plan><plan_id>retired-usd-900-30</plan_id><plan_name>Retired</plan_name><plan_amount>9.00</plan_amount></plan>
			<plan><plan_id>legacy-vip-plan</plan_id><plan_name>Legacy VIP</plan_name><plan_amount>5.00</plan_amount></plan>
		</nm_response>`))
	}))
	defer server.Close()

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, false)
	if err != nil {
		t.Fatalf("new nmi client: %v", err)
	}
	client.DirectPostURL = server.URL
	client.QueryURL = server.URL

	plans, err := fetchNMIPlans(client)
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

// -- archive: mode refusal -------------------------------------------------------

func TestArchiveCatalogExtras_RefusesUnderLimitedAndReadonly(t *testing.T) {
	for _, mode := range []string{config.ModeLimited, config.ModeReadOnly} {
		svc := &Service{rt: &app.Runtime{Config: &config.Config{Mode: mode}}}
		_, err := svc.ArchiveCatalogExtras(context.Background(), []CatalogExtra{
			{Provider: "stripe", ObjectType: "price", ExternalID: "price_x", Owned: true, Active: true},
		})
		if !errors.Is(err, ErrCatalogExtrasArchiveDisabled) {
			t.Errorf("mode=%s: expected ErrCatalogExtrasArchiveDisabled, got %v", mode, err)
		}
	}
}

func TestArchiveCatalogExtras_AllowedUnderFullMode(t *testing.T) {
	// Full mode passes the gate; with stripe unconfigured the only stripe
	// outcome is a per-object failure (not a refusal), proving the gate is
	// mode-driven, not config-driven.
	svc := &Service{rt: &app.Runtime{Config: &config.Config{Mode: config.ModeFull}}}
	outcomes, err := svc.ArchiveCatalogExtras(context.Background(), []CatalogExtra{
		{Provider: "mobius", ObjectType: "plan", ExternalID: "retired-usd-900-30", Owned: true, Active: true},
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(outcomes) != 1 || outcomes[0].Action != CatalogExtraManualActionRequired {
		t.Fatalf("expected manual_action_required for the NMI plan, got %+v", outcomes)
	}
}

// -- archive: per-provider calls + ownership guard --------------------------------

// fakeStripeArchiver records every archive call so tests can assert exactly
// which objects were touched and with what params.
type fakeStripeArchiver struct {
	productCalls map[string]catalog.UpdateProductParams
	priceCalls   map[string]catalog.UpdatePriceParams
	err          error
}

func newFakeStripeArchiver() *fakeStripeArchiver {
	return &fakeStripeArchiver{
		productCalls: map[string]catalog.UpdateProductParams{},
		priceCalls:   map[string]catalog.UpdatePriceParams{},
	}
}

func (f *fakeStripeArchiver) UpdateProduct(_ context.Context, id string, params catalog.UpdateProductParams) error {
	f.productCalls[id] = params
	return f.err
}

func (f *fakeStripeArchiver) UpdatePrice(_ context.Context, id string, params catalog.UpdatePriceParams) error {
	f.priceCalls[id] = params
	return f.err
}

func TestArchiveCatalogExtrasWith_ArchivesOnlyOwnedStripeObjects(t *testing.T) {
	archiver := newFakeStripeArchiver()
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_ours", Owned: true, Active: true},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_ours", Owned: true, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_foreign", Owned: false, Active: true},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_foreign", Owned: false, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_ours_inactive", Owned: true, Active: false},
		{Provider: "mobius", ObjectType: "plan", ExternalID: "retired-usd-900-30", Owned: true, Active: true},
		{Provider: "mobius", ObjectType: "plan", ExternalID: "legacy-vip-plan", Owned: false, Active: true},
	}
	outcomes, err := archiveCatalogExtrasWith(context.Background(), archiver, extras)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(outcomes) != len(extras) {
		t.Fatalf("expected %d outcomes, got %d", len(extras), len(outcomes))
	}

	// Exactly the OWNED ACTIVE stripe objects were archived, with active=false.
	if len(archiver.priceCalls) != 1 || len(archiver.productCalls) != 1 {
		t.Fatalf("expected exactly one price + one product archive call, got prices=%v products=%v", archiver.priceCalls, archiver.productCalls)
	}
	if p, ok := archiver.priceCalls["price_ours"]; !ok || p.Active == nil || *p.Active != false {
		t.Errorf("price_ours must be archived with active=false, got %+v", p)
	}
	if p, ok := archiver.productCalls["prod_ours"]; !ok || p.Active == nil || *p.Active != false {
		t.Errorf("prod_ours must be archived with active=false, got %+v", p)
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
	} {
		if got := byID[id].Action; got != wantAction {
			t.Errorf("%s: expected action %s, got %s", id, wantAction, got)
		}
	}
	// The NMI manual action carries the deletion-is-unsafe explanation.
	if d := byID["retired-usd-900-30"].Detail; !strings.Contains(d, "zero subscribers") {
		t.Errorf("NMI manual action detail must instruct verifying zero subscribers, got %q", d)
	}
}

func TestArchiveCatalogExtrasWith_ForeignNeverTouchedEvenOnExhaustive(t *testing.T) {
	archiver := newFakeStripeArchiver()
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_foreign", Owned: false, Active: true},
		{Provider: "stripe", ObjectType: "product", ExternalID: "prod_foreign", Owned: false, Active: true},
		{Provider: "mobius", ObjectType: "plan", ExternalID: "tenant-plan", Owned: false, Active: true},
	}
	outcomes, err := archiveCatalogExtrasWith(context.Background(), archiver, extras)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(archiver.priceCalls) != 0 || len(archiver.productCalls) != 0 {
		t.Fatalf("foreign objects must never receive archive calls: prices=%v products=%v", archiver.priceCalls, archiver.productCalls)
	}
	for _, o := range outcomes {
		if o.Action != CatalogExtraSkippedForeign {
			t.Errorf("%s: expected skipped_foreign, got %s", o.Extra.ExternalID, o.Action)
		}
	}
}

func TestArchiveCatalogExtrasWith_AggregatesWriteFailures(t *testing.T) {
	archiver := newFakeStripeArchiver()
	archiver.err = fmt.Errorf("stripe 500")
	extras := []CatalogExtra{
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_a", Owned: true, Active: true},
		{Provider: "stripe", ObjectType: "price", ExternalID: "price_b", Owned: true, Active: true},
	}
	outcomes, err := archiveCatalogExtrasWith(context.Background(), archiver, extras)
	if err == nil {
		t.Fatal("expected an aggregate error when archive writes fail")
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

// -- live smoke (read-only; opt-in) ------------------------------------------------

// TestLiveStripeExtrasListing runs the extras-detection LISTING (GETs only;
// nothing is ever archived) against a real Stripe account. Opt-in via
// OPENRAILS_LIVE_STRIPE_KEY; use a TEST-mode key.
func TestLiveStripeExtrasListing(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("OPENRAILS_LIVE_STRIPE_KEY"))
	if key == "" {
		t.Skip("set OPENRAILS_LIVE_STRIPE_KEY (a Stripe TEST key) to run the live read-only extras listing")
	}
	cfg := &config.Config{Processors: map[string]*config.ProcessorConfig{
		"stripe": {Type: config.ProcessorTypeStripe, SecretKey: key},
	}}
	lister := &catalog.StripeCatalogService{Config: cfg}
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
