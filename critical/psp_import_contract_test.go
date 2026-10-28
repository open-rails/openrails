//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// importWorld is a merchant whose PSPs are served by the provider fakes. The
// import reads them through OpenRails' own read-only clients; every case
// proves the fakes received no write.
type importWorld struct {
	*authorityWorld
	nmi    *nmimock.Mock
	stripe *stripemock.Mock
	ccbill *ccbillmock.Mock
	fleet  *river.Client[pgx.Tx]
}

func newImportWorld(t *testing.T, psps ...string) *importWorld {
	t.Helper()
	iw := &importWorld{}
	serve := func(handler http.Handler) loopbackWire {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		target, err := url.Parse(server.URL)
		require.NoError(t, err)
		wire := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(wire.CloseIdleConnections)
		return loopbackWire{target: target, wire: wire}
	}
	iw.authorityWorld = newAuthorityWorld(t, func(w *world) {
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		declared := openrails.PSPs{}
		ccbill := func(subacc string) openrails.PSPConfig {
			return openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: subacc, Environment: openrails.EnvironmentSandbox, Salt: "critical-ccbill-salt",
				DatalinkUsername: "critical-datalink", DatalinkPassword: "critical-datalink-password"}.PSPConfig()
		}
		for _, key := range psps {
			switch key {
			case "nmi":
				iw.nmi = nmimock.NewUnstarted(nmimock.Options{Clock: w.clock.Now})
				w.deps.NMITransport = serve(iw.nmi)
				declared[key] = openrails.NMIPSP{GatewayID: "critical-import-gateway", Environment: openrails.EnvironmentTest, SecurityKey: "critical-import-key",
					WebhookSigningSecret: "critical-import-webhook", TokenizationKey: "critical-import-collect"}.PSPConfig()
			case "stripe":
				iw.stripe = stripemock.NewUnstarted(stripemock.Options{Clock: w.clock.Now})
				w.deps.StripeTransport = serve(iw.stripe)
				declared[key] = openrails.StripePSP{AccountID: "acct_import", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_import",
					WebhookSigningSecret: "whsec_import", PublishableKey: "pk_test_import"}.PSPConfig()
			case "ccbill":
				iw.ccbill = ccbillmock.New()
				t.Cleanup(iw.ccbill.Close)
				w.cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{CCBillDataLinkURL: iw.ccbill.URL()}
				declared[key] = ccbill("0000")
			case "ccbill-two":
				declared[key] = ccbill("0001")
			}
		}
		merchant := w.cfg.Merchants["alpha"]
		merchant.PSPs = declared
		w.cfg.Merchants["alpha"] = merchant
	})
	t.Cleanup(func() {
		if iw.nmi != nil {
			require.Empty(t, iw.nmi.Calls(), "a PSP import never writes to NMI")
			require.Empty(t, iw.nmi.Unexpected())
		}
		if iw.stripe != nil {
			require.Empty(t, iw.stripe.Mutations(""), "a PSP import never writes to Stripe")
		}
		if iw.ccbill != nil {
			require.Empty(t, iw.ccbill.Calls(), "a PSP import never writes to CCBill")
			require.Empty(t, iw.ccbill.Unexpected())
		}
	})
	iw.fleet = startFleet(t, iw.world)
	return iw
}

// await is the import once its job reached status.
func (iw *importWorld) await(t *testing.T, id billing.PSPImportID, status billing.PSPImportStatus) *billing.PSPImport {
	t.Helper()
	var got *billing.PSPImport
	require.Eventually(t, func() bool {
		imp, err := iw.alphaRemote.GetPSPImport(t.Context(), id)
		if err != nil {
			return false
		}
		got = imp
		return imp.Status == status || imp.Status == billing.PSPImportFailed
	}, 60*time.Second, 50*time.Millisecond)
	require.Equal(t, status, got.Status, "import error: %v", got.Error)
	return got
}

func (iw *importWorld) pull(t *testing.T, params billing.CreatePSPImportParams) *billing.PSPImport {
	t.Helper()
	imp, err := iw.alphaRemote.CreatePSPImport(t.Context(), params)
	require.NoError(t, err)
	return iw.await(t, imp.ID, billing.PSPImportPulled)
}

func (iw *importWorld) apply(t *testing.T, id billing.PSPImportID) map[string]billing.PSPImportRecord {
	t.Helper()
	_, err := iw.alphaRemote.ApplyPSPImport(t.Context(), id)
	require.NoError(t, err)
	iw.await(t, id, billing.PSPImportApplied)
	return iw.records(t, id)
}

// records are the import's staged records by provider ref.
func (iw *importWorld) records(t *testing.T, id billing.PSPImportID) map[string]billing.PSPImportRecord {
	t.Helper()
	out := map[string]billing.PSPImportRecord{}
	page := billing.PSPImportRecordListParams{PageRequest: billing.PageRequest{Limit: 100}}
	for {
		got, err := iw.alphaRemote.ListPSPImportRecords(t.Context(), id, page)
		require.NoError(t, err)
		for _, r := range got.Items {
			out[r.Ref] = r
		}
		if got.Next == "" {
			return out
		}
		page.Cursor = got.Next
	}
}

func (iw *importWorld) catalog(t *testing.T, params billing.ProviderCatalogListParams) map[string]billing.ProviderCatalogObject {
	t.Helper()
	params.Limit = 100
	got, err := iw.alphaRemote.ListProviderCatalog(t.Context(), params)
	require.NoError(t, err)
	require.Empty(t, got.Next)
	out := map[string]billing.ProviderCatalogObject{}
	for _, o := range got.Items {
		out[string(o.Kind)+":"+o.Ref] = o
	}
	return out
}

func (iw *importWorld) psp(t *testing.T, key string) billing.PSPID {
	t.Helper()
	psps, err := iw.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	for _, p := range psps.Items {
		if p.Key == key {
			return p.ID
		}
	}
	t.Fatalf("PSP %s is not declared", key)
	return billing.PSPID{}
}

func (iw *importWorld) subscriptions(t *testing.T, customer billing.CustomerID) []billing.Subscription {
	t.Helper()
	subs, err := iw.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	return subs.Items
}

func (iw *importWorld) entitled(t *testing.T, customer billing.CustomerID, entitlement string) bool {
	t.Helper()
	access, err := iw.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}})
	require.NoError(t, err)
	return access.Entitlements[entitlement]
}

func customerOf(u authtest.User) billing.CustomerID { return billing.CustomerID(uuid.MustParse(u.ID)) }

// nmiMember is a vault holding one schedule on plan, paid once at start.
func (iw *importWorld) nmiMember(t *testing.T, email, plan, amount string, start time.Time) string {
	t.Helper()
	vault := iw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
	iw.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Email = email })
	schedule := iw.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: amount, Days: 30, Created: start, NextBilling: start.AddDate(0, 0, 30), Order: "order-" + plan + "-" + vault})
	iw.nmi.AddScheduleSale(schedule, start)
	return schedule
}

func refusalCode(t *testing.T, err error) (int, string) {
	t.Helper()
	require.Error(t, err)
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	return refusal.Status, refusal.Code
}

func TestPSPImport(t *testing.T) {
	t.Run("OR-J38-S01/unmapped-plan-blocks-until-mapped-and-lands-once", func(t *testing.T) {
		iw := newImportWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "silver", Name: "Silver", Amount: "10.00", Days: 30})
		gold := iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		silver := iw.nmiMember(t, iw.bob.Email, "silver", "10.00", start)
		since := start.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})

		plans := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Import: &imp.ID})
		require.Len(t, plans, 2)
		for ref, amount := range map[string]string{"gold": "20.00", "silver": "10.00"} {
			p := plans["price:"+ref]
			require.Nil(t, p.Price, "%s is unmapped", ref)
			require.False(t, p.Retired)
			require.Equal(t, amount, *p.Terms.StatedAmount, "NMI's own terms")
			require.Nil(t, p.Terms.Currency, "NMI names no currency")
			require.Equal(t, 720, *p.Terms.BillingIntervalHours)
			require.Equal(t, 1, *p.Subscriptions)
			require.Equal(t, "pull", p.ObservedVia)
		}

		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportBlocked, recs[gold].State)
		require.Contains(t, *recs[gold].Reason, `plan "gold" is unmapped`)
		require.Contains(t, *recs[silver].Reason, `plan "silver" is unmapped`)
		prices, err := iw.alpha.ListPrices(t.Context(), billing.PriceListParams{})
		require.NoError(t, err)
		require.Empty(t, prices.Items, "nothing creates a provider price by itself")
		require.Empty(t, iw.subscriptions(t, customerOf(iw.alice)))

		product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		require.False(t, iw.entitled(t, customerOf(iw.alice), "member:gold"))
		reads := iw.nmi.Reads()["v5:plans/{id}"]
		price, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)
		require.EqualValues(t, 20_000_000, price.UnitAmount)
		require.Equal(t, "USD", price.Currency)
		require.Equal(t, 720, *price.BillingIntervalHours)
		require.False(t, price.Archived)
		require.Equal(t, reads, iw.nmi.Reads()["v5:plans/{id}"], "mapping reads the observed plan, not NMI")

		_, err = iw.alphaRemote.LinkPSPImport(t.Context(), imp.ID, billing.LinkPSPImportParams{})
		require.NoError(t, err)
		recs = iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[gold].State, "%v", recs[gold].Reason)
		require.Equal(t, billing.PSPImportBlocked, recs[silver].State)
		subs := iw.subscriptions(t, customerOf(iw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		require.Equal(t, price.ID, subs[0].PriceID)
		require.Equal(t, gold, *subs[0].RailSubscriptionID)
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:gold"))

		recs = iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[gold].State)
		require.Len(t, iw.subscriptions(t, customerOf(iw.alice)), 1, "applying again lands nothing twice")

		iw.nmi.AddPlan(nmimock.Plan{ID: "bronze", Name: "Bronze", Amount: "5.00", Months: 1})
		reads = iw.nmi.Reads()["v5:plans/{id}"]
		bronze, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-bronze", PSP: "nmi", PlanID: "bronze", Archived: true})
		require.NoError(t, err)
		require.Equal(t, reads+1, iw.nmi.Reads()["v5:plans/{id}"], "a plan never observed is read once by id")
		require.Equal(t, 1, *bronze.BillingIntervalMonths)
		mapped := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Mapped: new(true)})
		require.Len(t, mapped, 2)
		require.Equal(t, bronze.ID, *mapped["price:bronze"].Price)
		require.Equal(t, "read", mapped["price:bronze"].ObservedVia)
		require.Equal(t, price.ID, *mapped["price:gold"].Price)
	})

	t.Run("OR-J38-S02/provider-inactive-price-is-not-a-change-target", func(t *testing.T) {
		iw := newImportWorld(t, "stripe")
		for _, p := range []struct {
			product, price string
			amount         int64
			inactive       bool
		}{{"prod_basic", "price_basic", 999, false}, {"prod_pro", "price_pro", 1999, false}, {"prod_old", "price_old", 1499, true}} {
			iw.stripe.AddProduct(p.product, p.product)
			iw.stripe.AddPrice(stripemock.Price{ID: p.price, Product: p.product, Amount: p.amount, Interval: "month", Inactive: p.inactive})
		}
		start := iw.clock.Now().Add(-10 * 24 * time.Hour)
		customer := iw.stripe.AddCustomer(iw.alice.Email, "Alice", nil)
		iw.stripe.AddSubscription(customer, "pm_alice", "price_basic", 999, start, start.AddDate(0, 1, 0))
		since := start.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since})
		all := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})
		require.Len(t, all, 6)
		require.True(t, all["price:price_old"].Retired)
		require.False(t, all["price:price_old"].Active)
		require.Equal(t, "prod_old", *all["price:price_old"].ProductRef)

		prices := map[string]*billing.Price{}
		for rank, key := range []string{"basic", "pro", "old"} {
			product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: new("membership"), TierRank: rank + 1, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
			prices[key], err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-" + key, PSP: "stripe", PriceID: "price_" + key})
			require.NoError(t, err)
		}
		require.Zero(t, iw.stripe.Reads("/v1/prices/"), "the pull listed inactive prices; nothing was read by id")
		require.EqualValues(t, 14_990_000, prices["old"].UnitAmount)
		require.True(t, prices["old"].Provider.Retired)
		require.False(t, prices["pro"].Provider.Retired)

		recs := iw.apply(t, imp.ID)
		subs := iw.subscriptions(t, customerOf(iw.alice))
		require.Len(t, subs, 1, "%+v", recs)
		require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		require.Equal(t, prices["basic"].ID, subs[0].PriceID)

		options := func() []billing.PriceID {
			var page billing.ListPage[billing.SubscriptionChangeOption]
			status, raw := iw.request(t, http.MethodGet, "/v1/me/subscriptions/"+subs[0].ID.String()+"/change/options", iw.aliceToken, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &page))
			var out []billing.PriceID
			for _, o := range page.Items {
				out = append(out, o.Price.ID)
			}
			return out
		}
		require.Equal(t, []billing.PriceID{prices["pro"].ID}, options())
		_, err := iw.alpha.ChangeSubscription(t.Context(), subs[0].ID, billing.ChangeSubscriptionParams{PriceID: &prices["old"].ID, Reason: "retired target", IdempotencyKey: "retired-target"})
		_, code := refusalCode(t, err)
		require.Equal(t, billing.CodeSubscriptionChangeTargetInactive, code)
		old, err := iw.alpha.GetPrice(t.Context(), prices["old"].ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.True(t, old.Provider.Retired)
		require.False(t, old.Archived, "retired is the provider's; archived stays ours")

		iw.stripe.SetPriceActive("price_pro", false)
		reads := iw.stripe.Reads("/v1/prices/")
		pro, err := iw.alpha.GetPrice(t.Context(), prices["pro"].ID, billing.GetPriceParams{Verify: true})
		require.NoError(t, err)
		require.Equal(t, reads+1, iw.stripe.Reads("/v1/prices/"), "verify reads the price once by id")
		require.True(t, pro.Provider.Retired)
		require.Equal(t, billing.SyncStatusInSync, pro.Provider.SyncStatus)
		require.Empty(t, options())
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:basic"), "retired never revokes access")
	})

	t.Run("OR-J38-S03/plan-less-ccbill-lands-on-one-exact-match-or-a-persons-pick", func(t *testing.T) {
		iw := newImportWorld(t, "ccbill", "ccbill-two", "nmi")
		flex := "681cb38f-afb9-4665-931f-2b896072178a"
		ccbillPrice := func(product, key, psp, rbo string, amount int64, archived bool) *billing.Price {
			t.Helper()
			price, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: product, Key: key, PSP: psp, FlexID: flex, RecurringBillingOptionID: rbo,
				UnitAmount: &amount, Currency: "USD", BillingIntervalHours: new(720), Archived: archived})
			require.NoError(t, err)
			return price
		}
		for _, key := range []string{"premium", "premium-legacy", "basic"} {
			_, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
		}
		premium := ccbillPrice("premium", "ccbill-23", "ccbill", "0000000023", 23_000_000, false)
		legacy := ccbillPrice("premium-legacy", "ccbill-19", "ccbill", "0000000019", 19_000_000, true)
		ccbillPrice("basic", "ccbill-999-a", "ccbill", "0000000999", 9_990_000, false)
		ccbillPrice("basic", "ccbill-999-b", "ccbill", "0000001999", 9_990_000, true)
		elsewhere := ccbillPrice("premium", "ccbill-two-23", "ccbill-two", "0000000023", 23_000_000, false)
		iw.nmi.AddPlan(nmimock.Plan{ID: "nmi-premium", Name: "NMI premium", Amount: "23.00", Days: 30})
		nmiPrice, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "premium", Key: "nmi-23", PSP: "nmi", PlanID: "nmi-premium"})
		require.NoError(t, err)

		carol, dave := authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth)
		signup := iw.clock.Now().Add(-40 * 24 * time.Hour)
		rebilled := iw.clock.Now().Add(-10 * 24 * time.Hour)
		next := rebilled.Add(30 * 24 * time.Hour)
		member := func(u authtest.User, rebill string) string {
			id := iw.ccbill.AddMember(ccbillmock.Member{Username: u.Username, Email: u.Email, Signup: signup, Rebill: &next})
			if rebill != "" {
				iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: id, At: rebilled, Amount: rebill})
			}
			return id
		}
		m23, m19, m999, none := member(iw.alice, "23.00"), member(iw.bob, "19.00"), member(carol, "9.99"), member(dave, "")
		since := rebilled.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since})
		require.Empty(t, iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP}), "CCBill has no catalog to observe")

		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[m23].State, "%v", recs[m23].Reason)
		require.Equal(t, billing.PSPImportImported, recs[m19].State, "%v", recs[m19].Reason)
		require.Equal(t, premium.ID, iw.subscriptions(t, customerOf(iw.alice))[0].PriceID)
		require.Equal(t, legacy.ID, iw.subscriptions(t, customerOf(iw.bob))[0].PriceID, "an archived price of another product still matches")
		require.True(t, iw.entitled(t, customerOf(iw.bob), "member:premium-legacy"))
		require.Equal(t, billing.PSPImportBlocked, recs[m999].State)
		require.Contains(t, *recs[m999].Reason, "2 match")
		require.Equal(t, billing.PSPImportBlocked, recs[none].State)
		require.Contains(t, *recs[none].Reason, "0 match")
		require.Empty(t, iw.subscriptions(t, customerOf(dave)))

		for _, wrong := range []billing.PriceID{nmiPrice.ID, elsewhere.ID} {
			_, err := iw.alphaRemote.LinkPSPImport(t.Context(), imp.ID, billing.LinkPSPImportParams{Prices: []billing.PSPImportPriceLink{{Record: recs[none].ID, Price: &wrong}}})
			status, code := refusalCode(t, err)
			require.Equal(t, http.StatusBadRequest, status)
			require.Equal(t, billing.CodeInvalidParam, code)
		}
		_, err = iw.alphaRemote.LinkPSPImport(t.Context(), imp.ID, billing.LinkPSPImportParams{Prices: []billing.PSPImportPriceLink{{Record: recs[none].ID, Price: &legacy.ID}}})
		require.NoError(t, err)
		recs = iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[none].State, "%v", recs[none].Reason)
		require.Equal(t, legacy.ID, *recs[none].ManualPrice)
		subs := iw.subscriptions(t, customerOf(dave))
		require.Len(t, subs, 1)
		require.Equal(t, legacy.ID, subs[0].PriceID)
		require.Equal(t, billing.PSPImportBlocked, recs[m999].State)
		require.Len(t, iw.subscriptions(t, customerOf(iw.alice)), 1)
	})

	t.Run("OR-J38-S04/pull-observes-the-whole-catalog-read-only", func(t *testing.T) {
		iw := newImportWorld(t, "nmi", "stripe")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "current", Name: "Current", Amount: "12.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "legacy", Name: "Legacy", Amount: "15.00", Days: 30})
		legacySub := iw.nmiMember(t, iw.alice.Email, "legacy", "15.00", start)
		iw.nmi.RemovePlan("legacy")
		since := start.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})

		named := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Import: &imp.ID})
		require.Len(t, named, 1)
		gone := named["price:legacy"]
		require.NotNil(t, gone.DeletedAt, "a by-id 404 records the plan gone")
		require.True(t, gone.Retired)
		require.Equal(t, "15.00", *gone.Terms.StatedAmount, "terms from the subscription's copy of its plan")
		require.Equal(t, 720, *gone.Terms.BillingIntervalHours)
		product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "legacy", DisplayName: "Legacy", Entitlements: []string{"member:legacy"}})
		require.NoError(t, err)
		price, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-legacy", PSP: "nmi", PlanID: "legacy", Archived: true})
		require.NoError(t, err, "a plan gone from the gateway still maps")
		require.True(t, price.Provider.Retired)
		require.EqualValues(t, 15_000_000, price.UnitAmount)
		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[legacySub].State, "%v", recs[legacySub].Reason)
		subs := iw.subscriptions(t, customerOf(iw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:legacy"), "retired keeps billing")

		iw.stripe.AddProduct("prod_inline", "Inline")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_inline", Product: "prod_inline", Amount: 1500, Interval: "month", Inline: true})
		customer := iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil)
		iw.stripe.AddSubscription(customer, "pm_bob", "price_inline", 1500, start, start.AddDate(0, 1, 0))
		stripeImp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since})
		require.Equal(t, 1, iw.stripe.Reads("/v1/prices/price_inline"), "an inline price is never listed: one by-id read observes it")
		inline := iw.catalog(t, billing.ProviderCatalogListParams{PSP: stripeImp.PSP, Import: &stripeImp.ID})
		require.Equal(t, "15.00", *inline["price:price_inline"].Terms.StatedAmount)
		require.Equal(t, "USD", *inline["price:price_inline"].Terms.Currency)
		require.Equal(t, 1, *inline["price:price_inline"].Subscriptions)

		first := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})
		require.Len(t, first, 2)
		iw.clock.Advance(time.Hour)
		again := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		second := iw.catalog(t, billing.ProviderCatalogListParams{PSP: again.PSP})
		require.Len(t, second, 2, "a second pull adds no rows")
		for key, o := range second {
			require.True(t, o.ObservedAt.After(first[key].ObservedAt), "%s observed again", key)
		}
		require.Equal(t, first["price:legacy"].DeletedAt, second["price:legacy"].DeletedAt, "deleted when first found gone")

		keys := func(objects map[string]billing.ProviderCatalogObject) []string {
			out := make([]string, 0, len(objects))
			for k := range objects {
				out = append(out, k)
			}
			slices.Sort(out)
			return out
		}
		require.Equal(t, []string{"price:legacy"}, keys(iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Mapped: new(true)})))
		require.Equal(t, []string{"price:current"}, keys(iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Mapped: new(false)})))
		require.Equal(t, []string{"price:legacy"}, keys(iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Retired: new(true)})))
		require.Equal(t, []string{"price:current"}, keys(iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Retired: new(false)})))
		require.Equal(t, []string{"price:price_inline", "product:prod_inline"}, keys(iw.catalog(t, billing.ProviderCatalogListParams{PSP: iw.psp(t, "stripe")})))
		_, err = iw.alphaRemote.ListProviderCatalog(t.Context(), billing.ProviderCatalogListParams{PSP: imp.PSP, Import: &stripeImp.ID})
		status, code := refusalCode(t, err)
		require.Equal(t, http.StatusBadRequest, status, "an import lists only its own PSP's plans")
		require.Equal(t, billing.CodeInvalidParam, code)
	})
}
