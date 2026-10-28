//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
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
	// api is the merchant's client; a restart replaces it with the new
	// engine's own.
	api *openrails.Client
	// importable are the declared PSPs an onboarding import pulls.
	importable []string
	// declared are the PSPs the world's fakes serve.
	declared openrails.PSPs
}

func importNMIPSP(gateway, securityKey string, archived bool) openrails.PSPConfig {
	return openrails.NMIPSP{GatewayID: gateway, Environment: openrails.EnvironmentTest, SecurityKey: securityKey, WebhookSigningSecret: "critical-import-webhook",
		TokenizationKey: "critical-import-collect", Archived: archived}.PSPConfig()
}

func importStripePSP(secretKey string) openrails.PSPConfig {
	return openrails.StripePSP{AccountID: "acct_import", Environment: openrails.EnvironmentTest, SecretKey: secretKey, WebhookSigningSecret: "whsec_import",
		PublishableKey: "pk_test_import"}.PSPConfig()
}

func importCCBillPSP(subacc, password string) openrails.PSPConfig {
	return openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: subacc, Environment: openrails.EnvironmentSandbox, Salt: "critical-ccbill-salt",
		DatalinkUsername: "critical-datalink", DatalinkPassword: password}.PSPConfig()
}

// deadRPC is a loopback Solana RPC nothing answers.
const deadRPC = "http://127.0.0.1:1"

// bootImportWorld boots the merchant with psps declared. Its configuration
// load queues each importable PSP's onboarding import; no fleet runs them
// until start.
func bootImportWorld(t *testing.T, psps ...string) *importWorld {
	t.Helper()
	return bootImportWorldDeclaring(t, true, psps...)
}

// bootImportWorldDeclaring is bootImportWorld whose engine declares the PSPs
// (in declared either way) only when declare is set.
func bootImportWorldDeclaring(t *testing.T, declare bool, psps ...string) *importWorld {
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
		sandbox := openrails.ProviderSandboxConfig{}
		for _, key := range psps {
			switch key {
			case "nmi":
				iw.nmi = nmimock.NewUnstarted(nmimock.Options{Clock: w.clock.Now})
				w.deps.NMITransport = serve(iw.nmi)
				declared[key] = importNMIPSP("critical-import-gateway", "critical-import-key", false)
			case "nmi-archived":
				declared[key] = importNMIPSP("critical-archived-gateway", "critical-archived-key", true)
				continue
			case "stripe":
				iw.stripe = stripemock.NewUnstarted(stripemock.Options{Clock: w.clock.Now})
				w.deps.StripeTransport = serve(iw.stripe)
				declared[key] = importStripePSP("sk_test_import")
			case "ccbill":
				iw.ccbill = ccbillmock.New()
				t.Cleanup(iw.ccbill.Close)
				sandbox.CCBillDataLinkURL = iw.ccbill.URL()
				declared[key] = importCCBillPSP("0000", "critical-datalink-password")
			case "ccbill-two":
				declared[key] = importCCBillPSP("0001", "critical-datalink-password")
			case "solana":
				sandbox.SolanaRPCURL = deadRPC
				declared[key] = openrails.SolanaPSP{Environment: openrails.EnvironmentDevnet, Treasury: solanago.NewWallet().PublicKey().String(), RPCURL: deadRPC}.PSPConfig()
				continue
			}
			iw.importable = append(iw.importable, key)
		}
		if sandbox != (openrails.ProviderSandboxConfig{}) {
			w.cfg.ProviderSandbox = &sandbox
		}
		iw.declared = declared
		if declare {
			merchant := w.cfg.Merchants["alpha"]
			merchant.PSPs = declared
			w.cfg.Merchants["alpha"] = merchant
		}
	})
	iw.api = iw.alphaRemote
	t.Cleanup(func() {
		if iw.nmi != nil {
			require.Empty(t, iw.nmi.Calls(), "a PSP import never writes to NMI")
			require.Empty(t, iw.nmi.Unexpected())
		}
		if iw.stripe != nil {
			require.Empty(t, iw.stripe.Mutations(""), "a PSP import never writes to Stripe")
		}
		if iw.ccbill != nil {
			require.Empty(t, iw.ccbill.Mutations(), "a PSP import never writes to CCBill")
			require.Empty(t, iw.ccbill.Unexpected())
		}
	})
	iw.fleet = hostFleet(t, iw.world, 2)
	return iw
}

// newImportWorld is a booted import world whose fleet ran every onboarding
// pull: of PSPs that hold nothing yet.
func newImportWorld(t *testing.T, psps ...string) *importWorld {
	t.Helper()
	iw := bootImportWorld(t, psps...)
	iw.start(t)
	for _, key := range iw.importable {
		iw.await(t, iw.onboarding(t, key).ID, billing.PSPImportPulled)
	}
	return iw
}

// start runs the world's fleet.
func (iw *importWorld) start(t *testing.T) {
	t.Helper()
	require.NoError(t, iw.engine.Start(t.Context(), openrails.WithRiverClient(iw.fleet)))
	require.NoError(t, iw.fleet.Start(t.Context()))
}

// restart stops the fleet and the engine, lets change redeclare PSPs, and
// boots a new engine and fleet on the same database: a new process loading
// the configuration first.
func (iw *importWorld) restart(t *testing.T, change func(openrails.PSPs)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, iw.fleet.StopAndCancel(ctx))
	require.NoError(t, iw.engine.Close(ctx))
	if change != nil {
		change(iw.cfg.Merchants["alpha"].PSPs)
	}
	var err error
	iw.engine, err = openrails.New(t.Context(), iw.cfg, iw.deps)
	require.NoError(t, err)
	iw.alpha, err = iw.engine.MerchantBySlug("alpha")
	require.NoError(t, err)
	iw.api = iw.alpha
	iw.fleet = hostFleet(t, iw.world, 2)
	iw.start(t)
}

// onboarding is the PSP's one onboarding import, once its configuration load
// created it.
func (iw *importWorld) onboarding(t *testing.T, key string) *billing.PSPImport {
	t.Helper()
	var got []billing.PSPImport
	require.Eventually(t, func() bool {
		page, err := iw.api.ListPSPImports(t.Context(), billing.PSPImportListParams{PSP: billing.PSPRef{Key: key}, Origin: billing.PSPImportOnboarding})
		if err != nil {
			return false
		}
		got = page.Items
		return len(got) > 0
	}, 20*time.Second, 20*time.Millisecond, "%s has no onboarding import", key)
	require.Len(t, got, 1, "one onboarding import per PSP")
	require.Equal(t, billing.PSPImportOnboarding, got[0].Origin)
	return &got[0]
}

// onboardings are the merchant's onboarding imports by PSP id.
func (iw *importWorld) onboardings(t *testing.T) map[billing.PSPID][]billing.PSPImport {
	t.Helper()
	page, err := iw.api.ListPSPImports(t.Context(), billing.PSPImportListParams{Origin: billing.PSPImportOnboarding, PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	out := map[billing.PSPID][]billing.PSPImport{}
	for _, imp := range page.Items {
		out[imp.PSP] = append(out[imp.PSP], imp)
	}
	return out
}

// jobs are the River jobs that ran or will run steps of the import.
func (iw *importWorld) jobs(t *testing.T, id billing.PSPImportID) []*rivertype.JobRow {
	t.Helper()
	all, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import").First(1000))
	require.NoError(t, err)
	var out []*rivertype.JobRow
	for _, job := range all.Jobs {
		var args struct {
			ImportID uuid.UUID `json:"import_id"`
		}
		require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
		if args.ImportID == id.UUID() {
			out = append(out, job)
		}
	}
	return out
}

// await is the import once its job reached status.
func (iw *importWorld) await(t *testing.T, id billing.PSPImportID, status billing.PSPImportStatus) *billing.PSPImport {
	t.Helper()
	var got *billing.PSPImport
	require.Eventually(t, func() bool {
		imp, err := iw.api.GetPSPImport(t.Context(), id)
		if err != nil {
			return false
		}
		got = imp
		return imp.Status == status || imp.Status == billing.PSPImportFailed
	}, 60*time.Second, 50*time.Millisecond)
	var why, code string
	if got.Error != nil {
		why = *got.Error
	}
	if got.ErrorCode != nil {
		code = string(*got.ErrorCode)
	}
	require.Equal(t, status, got.Status, "import error %s: %s", code, why)
	return got
}

func (iw *importWorld) pull(t *testing.T, params billing.CreatePSPImportParams) *billing.PSPImport {
	t.Helper()
	imp, err := iw.api.CreatePSPImport(t.Context(), params)
	require.NoError(t, err)
	require.Equal(t, billing.PSPImportManual, imp.Origin)
	return iw.await(t, imp.ID, billing.PSPImportPulled)
}

func (iw *importWorld) apply(t *testing.T, id billing.PSPImportID) map[string]billing.PSPImportRecord {
	t.Helper()
	_, err := iw.api.ApplyPSPImport(t.Context(), id)
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
		got, err := iw.api.ListPSPImportRecords(t.Context(), id, page)
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
	got, err := iw.api.ListProviderCatalog(t.Context(), params)
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

// cancelOnboarding cancels the queued onboarding pulls, once the
// configuration loads queued all n, for a scenario whose provider reads they
// would disturb.
func cancelOnboarding(t *testing.T, fleet *river.Client[pgx.Tx], n int) {
	t.Helper()
	var jobs []*rivertype.JobRow
	require.Eventually(t, func() bool {
		list, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import"))
		if err != nil {
			return false
		}
		jobs = list.Jobs
		return len(jobs) >= n
	}, 20*time.Second, 20*time.Millisecond, "the configuration loads queue %d onboarding pulls", n)
	for _, job := range jobs {
		_, err := fleet.JobCancel(t.Context(), job.ID)
		require.NoError(t, err)
	}
}

// answer is a provider's reply to r.
func answer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// refuseNMI makes the gateway refuse key as NMI does: v5 answers 401, the
// Query API an authentication error.
func refuseNMI(m *nmimock.Mock, key string) {
	keyOf := func(r *http.Request) (string, bool) {
		if header := r.Header.Get("Authorization"); header != "" {
			return header, true
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		form, _ := url.ParseQuery(string(body))
		return form.Get("security_key"), false
	}
	m.Intercept(func(r *http.Request) bool { k, _ := keyOf(r); return k == key }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
		if _, v5 := keyOf(r); v5 {
			return answer(r, http.StatusUnauthorized, `{"type":"error","error_code":"unauthorized","message":"Authentication failed"}`), nil
		}
		return answer(r, http.StatusOK, `<?xml version="1.0" encoding="UTF-8"?><nm_response><error_response>Authentication Failed</error_response></nm_response>`), nil
	})
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
		_, err := iw.alphaRemote.LinkPSPImport(t.Context(), imp.ID, billing.LinkPSPImportParams{Prices: []billing.PSPImportPriceLink{{Record: recs[*subs[0].RailSubscriptionID].ID, Price: &prices["pro"].ID}}})
		status, code := refusalCode(t, err)
		require.Equal(t, http.StatusBadRequest, status, "a Stripe subscription lands on its own price, never a pick")
		require.Equal(t, billing.CodeInvalidParam, code)

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
		_, err = iw.alpha.ChangeSubscription(t.Context(), subs[0].ID, billing.ChangeSubscriptionParams{PriceID: &prices["old"].ID, Reason: "retired target", IdempotencyKey: "retired-target"})
		_, code = refusalCode(t, err)
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
		ccbillPrice := func(product, key, psp, rbo string, amount int64, archived bool, hours int, oneOff bool) *billing.Price {
			t.Helper()
			price, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: product, Key: key, PSP: psp, FlexID: flex, RecurringBillingOptionID: rbo,
				UnitAmount: &amount, Currency: "USD", BillingIntervalHours: new(hours), Archived: archived, OneOff: oneOff})
			require.NoError(t, err)
			return price
		}
		for _, key := range []string{"premium", "premium-legacy", "basic", "lite"} {
			_, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
		}
		premium := ccbillPrice("premium", "ccbill-23", "ccbill", "0000000023", 23_000_000, false, 720, false)
		ccbillPrice("premium", "ccbill-23-weekly", "ccbill", "0000002023", 23_000_000, false, 168, false)
		legacy := ccbillPrice("premium-legacy", "ccbill-19", "ccbill", "0000000019", 19_000_000, true, 720, false)
		ccbillPrice("basic", "ccbill-999-a", "ccbill", "0000000999", 9_990_000, false, 720, false)
		ccbillPrice("basic", "ccbill-999-b", "ccbill", "0000001999", 9_990_000, true, 720, false)
		lite := ccbillPrice("lite", "ccbill-lite", "ccbill", "0000000005", 5_000_000, false, 720, false)
		once := ccbillPrice("lite", "ccbill-lite-once", "ccbill", "0000001005", 5_000_000, false, 720, true)
		require.Nil(t, once.BillingIntervalHours, "a one-off price does not recur")
		elsewhere := ccbillPrice("premium", "ccbill-two-23", "ccbill-two", "0000000023", 23_000_000, false, 720, false)
		iw.nmi.AddPlan(nmimock.Plan{ID: "nmi-premium", Name: "NMI premium", Amount: "23.00", Days: 30})
		nmiPrice, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "premium", Key: "nmi-23", PSP: "nmi", PlanID: "nmi-premium"})
		require.NoError(t, err)

		carol, dave, erin, frank := authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth)
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
		m23, m19, m999, none, m5, m14 := member(iw.alice, "23.00"), member(iw.bob, "19.00"), member(carol, "9.99"), member(dave, ""), member(erin, "5.00"), member(frank, "14.00")
		// DataLink states no next date for this member: its cadence is unknown.
		gina := authtest.NewUser(t, iw.auth)
		undated := iw.ccbill.AddMember(ccbillmock.Member{Username: gina.Username, Email: gina.Email, Signup: signup})
		iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: undated, At: rebilled, Amount: "5.00"})
		since := rebilled.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since})
		require.Empty(t, iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP}), "CCBill has no catalog to observe")

		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[m23].State, "%v", recs[m23].Reason)
		require.Equal(t, premium.ID, iw.subscriptions(t, customerOf(iw.alice))[0].PriceID, "the weekly 23.00 price does not rebill every 30 days")
		require.Equal(t, billing.PSPImportImported, recs[m19].State, "%v", recs[m19].Reason)
		require.Equal(t, legacy.ID, iw.subscriptions(t, customerOf(iw.bob))[0].PriceID, "an archived price of another product still matches")
		require.True(t, iw.entitled(t, customerOf(iw.bob), "member:premium-legacy"))
		require.Equal(t, billing.PSPImportImported, recs[m5].State, "%v", recs[m5].Reason)
		require.Equal(t, lite.ID, iw.subscriptions(t, customerOf(erin))[0].PriceID, "a one-off price at the same amount never matches a rebill")
		require.Equal(t, billing.PSPImportImported, recs[undated].State, "%v", recs[undated].Reason)
		require.Equal(t, lite.ID, iw.subscriptions(t, customerOf(gina))[0].PriceID, "only recurring prices match, whatever the cadence")
		require.Equal(t, billing.PSPImportBlocked, recs[m999].State)
		require.Contains(t, *recs[m999].Reason, "2 match")
		require.Equal(t, billing.PSPImportBlocked, recs[m14].State)
		require.Contains(t, *recs[m14].Reason, "latest rebill 14.00 every 30 days: 0 match")
		require.Equal(t, billing.PSPImportBlocked, recs[none].State)
		require.Contains(t, *recs[none].Reason, "no rebill: 0 match")
		require.Empty(t, iw.subscriptions(t, customerOf(dave)))

		pick := func(id billing.PSPImportID, record billing.PSPImportRecordID, price *billing.PriceID) error {
			_, err := iw.alphaRemote.LinkPSPImport(t.Context(), id, billing.LinkPSPImportParams{Prices: []billing.PSPImportPriceLink{{Record: record, Price: price}}})
			return err
		}
		refused := func(err error) {
			t.Helper()
			status, code := refusalCode(t, err)
			require.Equal(t, http.StatusBadRequest, status)
			require.Equal(t, billing.CodeInvalidParam, code)
		}
		for _, wrong := range []billing.PriceID{nmiPrice.ID, elsewhere.ID, once.ID} {
			refused(pick(imp.ID, recs[none].ID, &wrong))
		}
		var rebill billing.PSPImportRecord
		for _, r := range recs {
			if r.Kind == billing.PSPImportTransaction && r.SubscriptionRef != nil && *r.SubscriptionRef == m23 {
				rebill = r
			}
		}
		require.False(t, rebill.ID.IsZero())
		refused(pick(imp.ID, rebill.ID, &premium.ID))
		nmiImport := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		refused(pick(nmiImport.ID, recs[none].ID, &premium.ID))

		require.NoError(t, pick(imp.ID, recs[m14].ID, &premium.ID))
		require.Equal(t, premium.ID, *iw.records(t, imp.ID)[m14].ManualPrice)
		require.NoError(t, pick(imp.ID, recs[m14].ID, nil))
		require.Nil(t, iw.records(t, imp.ID)[m14].ManualPrice, "a null price clears the pick")
		require.NoError(t, pick(imp.ID, recs[none].ID, &legacy.ID))
		recs = iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[none].State, "%v", recs[none].Reason)
		require.Equal(t, legacy.ID, *recs[none].ManualPrice)
		subs := iw.subscriptions(t, customerOf(dave))
		require.Len(t, subs, 1)
		require.Equal(t, legacy.ID, subs[0].PriceID)
		require.Equal(t, billing.PSPImportBlocked, recs[m14].State, "a cleared pick lands nothing")
		require.Empty(t, iw.subscriptions(t, customerOf(frank)))
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

		// Provider prices mapped before the catalog was observed have no row
		// (fixture: a database from before the observed catalog). Their plan
		// vanishing still retires them, by verify or by the next pull.
		spare, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "spare", DisplayName: "Spare", Entitlements: []string{"member:spare"}})
		require.NoError(t, err)
		iw.nmi.AddPlan(nmimock.Plan{ID: "spare", Name: "Spare", Amount: "8.00", Days: 30})
		mapped := map[string]*billing.Price{}
		for _, plan := range []string{"current", "spare"} {
			mapped[plan], err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: spare.ID, Key: "nmi-" + plan, PSP: "nmi", PlanID: plan, Archived: true})
			require.NoError(t, err)
		}
		// A 404 without NMI's own not-found answer (a proxy, a moved route) is
		// no evidence the plan is gone.
		iw.nmi.FailRequests(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/plans/current") }, http.StatusNotFound, 1)
		unknown, err := iw.alpha.GetPrice(t.Context(), mapped["current"].ID, billing.GetPriceParams{Verify: true})
		require.NoError(t, err)
		require.Equal(t, billing.SyncStatusUnknown, unknown.Provider.SyncStatus)
		require.False(t, unknown.Provider.Retired)
		require.Nil(t, iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})["price:current"].DeletedAt)
		for _, plan := range []string{"current", "spare"} {
			_, err = iw.pool.Exec(t.Context(), "DELETE FROM "+pgx.Identifier{iw.schema, "provider_catalog_objects"}.Sanitize()+" WHERE ref = $1", plan)
			require.NoError(t, err)
			iw.nmi.RemovePlan(plan)
		}
		verified, err := iw.alpha.GetPrice(t.Context(), mapped["spare"].ID, billing.GetPriceParams{Verify: true})
		require.NoError(t, err)
		require.Equal(t, billing.SyncStatusMissing, verified.Provider.SyncStatus)
		require.True(t, verified.Provider.Retired)
		stored, err := iw.alpha.GetPrice(t.Context(), mapped["spare"].ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.True(t, stored.Provider.Retired, "verify records the vanished plan from the price's own terms")
		require.Equal(t, "read", iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})["price:spare"].ObservedVia)
		require.NotContains(t, iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP}), "price:current", "not yet read again: nothing says it is gone")
		iw.clock.Advance(time.Hour)
		iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		retired := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Retired: new(true)})
		for plan, amount := range map[string]string{"current": "12.00", "spare": "8.00"} {
			require.NotNil(t, retired["price:"+plan].DeletedAt, plan)
			require.Equal(t, amount, *retired["price:"+plan].Terms.StatedAmount, plan)
			price, err := iw.alpha.GetPrice(t.Context(), mapped[plan].ID, billing.GetPriceParams{})
			require.NoError(t, err)
			require.True(t, price.Provider.Retired, "%s: the pull reads every bound plan", plan)
		}
	})

	t.Run("OR-J38-S05/stripe-catalog-keeps-the-newest-truth-and-skips-what-it-cannot-read", func(t *testing.T) {
		iw := newImportWorld(t, "stripe")
		for _, product := range []string{"prod_a", "prod_c", "prod_d", "prod_odd"} {
			iw.stripe.AddProduct(product, product)
		}
		for _, p := range []stripemock.Price{
			{ID: "price_a", Product: "prod_a", Amount: 999, Interval: "month"},
			{ID: "price_c", Product: "prod_c", Amount: 1999, Interval: "month"},
			{ID: "price_d", Product: "prod_d", Amount: 2999, Interval: "month", Inactive: true},
			{ID: "price_xyz", Product: "prod_odd", Currency: "xyz", Amount: 500, Interval: "month"},
			{ID: "price_custom", Product: "prod_odd", Custom: true, Interval: "month"},
			{ID: "price_flip", Product: "prod_odd", Amount: 700, Interval: "month"},
			{ID: "price_unread", Product: "prod_odd", Amount: 800, Interval: "month", Inline: true},
		} {
			iw.stripe.AddPrice(p)
		}
		start := iw.clock.Now().Add(-10 * 24 * time.Hour)
		iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.alice.Email, "Alice", nil), "pm_alice", "price_a", 999, start, start.AddDate(0, 1, 0))
		unread := iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil), "pm_bob", "price_unread", 800, start, start.AddDate(0, 1, 0))
		answer := func(r *http.Request, status int, body string) *http.Response {
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", "application/json")
			rec.WriteHeader(status)
			_, _ = rec.WriteString(body)
			res := rec.Result()
			res.Request = r
			return res
		}
		priceList := func(active string) func(*http.Request) bool {
			return func(r *http.Request) bool {
				return r.Method == http.MethodGet && r.URL.Path == "/v1/prices" && r.URL.Query().Get("active") == active
			}
		}
		// Stripe deactivates price_flip between the active and the inactive
		// list reads, and cannot answer for price_unread.
		var flip sync.Once
		iw.stripe.Intercept(priceList("false"), func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
			flip.Do(func() { iw.stripe.SetPriceActive("price_flip", false) })
			return serve(), nil
		})
		iw.stripe.Intercept(func(r *http.Request) bool { return r.URL.Path == "/v1/prices/price_unread" }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
			return answer(r, http.StatusServiceUnavailable, `{"error":{"type":"api_error","message":"unavailable"}}`), nil
		})
		since := start.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since})
		all := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})
		require.Nil(t, all["price:price_xyz"].Terms.StatedAmount, "an unsupported currency leaves no amount")
		require.Equal(t, "XYZ", *all["price:price_xyz"].Terms.Currency)
		require.Nil(t, all["price:price_custom"].Terms.StatedAmount)
		require.False(t, all["price:price_flip"].Active, "seen active, then inactive: the later read wins")
		require.True(t, all["price:price_flip"].Retired)
		require.NotContains(t, all, "price:price_unread", "an object that cannot be read is left for the next read")

		_, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "odd", DisplayName: "odd", Entitlements: []string{"member:odd"}})
		require.NoError(t, err)
		for ref, reason := range map[string]string{"price_xyz": "XYZ, which OpenRails does not support", "price_custom": "no fixed amount"} {
			_, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: "odd", Key: ref, PSP: "stripe", PriceID: ref})
			status, code := refusalCode(t, err)
			require.Equal(t, http.StatusBadRequest, status)
			require.Equal(t, billing.CodeInvalidParam, code)
			require.ErrorContains(t, err, reason)
		}
		prices := map[string]*billing.Price{}
		for _, key := range []string{"a", "c", "d"} {
			product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
			prices[key], err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-" + key, PSP: "stripe", PriceID: "price_" + key, Archived: true})
			require.NoError(t, err)
		}
		recs := iw.apply(t, imp.ID)
		require.Len(t, iw.subscriptions(t, customerOf(iw.alice)), 1, "the pull staged the roster despite the odd prices")
		require.Contains(t, *recs[unread].Reason, `plan "price_unread" is unmapped`)

		verify := func(key string) *billing.Price {
			t.Helper()
			price, err := iw.alpha.GetPrice(t.Context(), prices[key].ID, billing.GetPriceParams{Verify: true})
			require.NoError(t, err)
			return price
		}
		iw.stripe.SetProductActive("prod_a", false)
		require.True(t, verify("a").Provider.Retired, "a price read by id refreshes its product")
		iw.stripe.DeleteProduct("prod_c")
		require.True(t, verify("c").Provider.Retired)
		products := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP, Kind: billing.ProviderCatalogProduct})
		require.False(t, products["product:prod_a"].Active)
		require.NotNil(t, products["product:prod_c"].DeletedAt)

		iw.stripe.ClearIntercepts()
		iw.stripe.Intercept(func(r *http.Request) bool { return r.URL.Path == "/v1/prices/price_d" }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
			return answer(r, http.StatusNotFound, `{"error":{"type":"invalid_request_error","message":"Unrecognized request URL"}}`), nil
		})
		require.Equal(t, billing.SyncStatusUnknown, verify("d").Provider.SyncStatus, "a 404 that is not resource_missing is no evidence")
		require.Nil(t, iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})["price:price_d"].DeletedAt)
		iw.stripe.ClearIntercepts()

		// A pull's list read before a later by-id read never overwrites it.
		held := iw.stripe.Hold(priceList("false"), stripemock.HoldResponse)
		again, err := iw.alphaRemote.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since})
		require.NoError(t, err)
		select {
		case <-held.Arrived():
		case <-time.After(30 * time.Second):
			t.Fatal("the pull never listed inactive prices")
		}
		iw.stripe.SetPriceActive("price_d", true)
		iw.clock.Advance(time.Minute)
		require.False(t, verify("d").Provider.Retired)
		held.Release()
		iw.await(t, again.ID, billing.PSPImportPulled)
		d := iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP})["price:price_d"]
		require.True(t, d.Active, "the older list observation lost to the newer read")
		require.Equal(t, "read", d.ObservedVia)
		require.Contains(t, iw.catalog(t, billing.ProviderCatalogListParams{PSP: imp.PSP}), "price:price_unread", "read once Stripe answers")
	})

	t.Run("OR-J38-S06/onboarding-pull-per-rail", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi", "nmi-archived", "stripe", "ccbill", "solana")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		nmiSub := iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		// Bob's Stripe customer is older than his subscription: history reads
		// from it.
		created := iw.clock.Now().Truncate(time.Second)
		cus := iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil)
		iw.clock.Advance(2 * time.Hour)
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		stripeSub := iw.stripe.AddSubscription(cus, "pm_bob", "price_basic", 999, created.Add(time.Hour), created.Add(time.Hour).AddDate(0, 1, 0))
		carol := authtest.NewUser(t, iw.auth)
		next := iw.clock.Now().Add(20 * 24 * time.Hour)
		member := iw.ccbill.AddMember(ccbillmock.Member{Username: carol.Username, Email: carol.Email, Signup: start, Rebill: &next})
		rebill := iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: member, At: start.Add(time.Hour), Amount: "23.00"})
		iw.clock.Advance(time.Minute)
		iw.start(t)

		imports := map[string]*billing.PSPImport{}
		for _, key := range []string{"nmi", "stripe", "ccbill"} {
			imports[key] = iw.await(t, iw.onboarding(t, key).ID, billing.PSPImportPulled)
			require.Nil(t, imports[key].ErrorCode)
		}
		all := iw.onboardings(t)
		require.ElementsMatch(t, []billing.PSPID{imports["nmi"].PSP, imports["stripe"].PSP, imports["ccbill"].PSP}, slices.Collect(maps.Keys(all)),
			"one onboarding import per live importable PSP; none for Solana or the archived NMI account")
		require.Equal(t, start, imports["nmi"].HistorySince.UTC())
		require.Equal(t, created, imports["stripe"].HistorySince.UTC(), "history starts at the earliest object listed, a customer included")
		require.Equal(t, start, imports["ccbill"].HistorySince.UTC())

		linked := func(recs map[string]billing.PSPImportRecord, ref string, who billing.CustomerID) {
			t.Helper()
			r, ok := recs[ref]
			require.True(t, ok, "%s is staged", ref)
			require.Equal(t, billing.PSPImportLinked, r.State, "%s: %v", ref, r.Reason)
			require.Equal(t, who, *r.Customer)
		}
		nmiRecs := iw.records(t, imports["nmi"].ID)
		linked(nmiRecs, nmiSub, customerOf(iw.alice))
		var sales int
		for _, r := range nmiRecs {
			if r.Kind == billing.PSPImportTransaction {
				sales++
				require.Equal(t, customerOf(iw.alice), *r.Customer)
			}
		}
		require.Equal(t, 1, sales, "the schedule's sale is in the history read")
		stripeRecs := iw.records(t, imports["stripe"].ID)
		linked(stripeRecs, cus, customerOf(iw.bob))
		linked(stripeRecs, stripeSub, customerOf(iw.bob))
		ccbillRecs := iw.records(t, imports["ccbill"].ID)
		linked(ccbillRecs, member, customerOf(carol))
		linked(ccbillRecs, rebill, customerOf(carol))

		for _, who := range []billing.CustomerID{customerOf(iw.alice), customerOf(iw.bob), customerOf(carol)} {
			require.Empty(t, iw.subscriptions(t, who), "the pull lands nothing")
			payments, err := iw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: who})
			require.NoError(t, err)
			require.Empty(t, payments.Items)
			methods, err := iw.alpha.ListPaymentMethods(t.Context(), who, billing.PaymentMethodListParams{})
			require.NoError(t, err)
			require.Empty(t, methods.Items)
		}

		// A person deletes the Stripe onboarding import and imports by hand: the
		// next process to load the configuration makes the onboarding import
		// again, and the manual one does not stand in for it.
		require.NoError(t, iw.api.DeletePSPImport(t.Context(), imports["stripe"].ID))
		manual := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}})
		replica, err := openrails.New(t.Context(), iw.cfg, iw.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, replica.Close(context.Background())) })
		again := iw.await(t, iw.onboarding(t, "stripe").ID, billing.PSPImportPulled)
		require.NotEqual(t, imports["stripe"].ID, again.ID)
		require.NotEqual(t, manual.ID, again.ID)
		for _, key := range []string{"nmi", "ccbill"} {
			require.Equal(t, imports[key].ID, iw.onboarding(t, key).ID, "a live onboarding import is never made twice")
		}
	})

	t.Run("OR-J38-S07/two-runtimes-one-database", func(t *testing.T) {
		// The first process declares none of the PSPs: each of the processes
		// below loads them first, and they race to create the imports.
		iw := bootImportWorldDeclaring(t, false, "nmi", "stripe", "ccbill")
		merchant := iw.cfg.Merchants["alpha"]
		merchant.PSPs = iw.declared
		iw.cfg.Merchants["alpha"] = merchant
		// state is one onboarding import and one pull job per PSP, or why not.
		state := func() error {
			page, err := iw.api.ListPSPImports(t.Context(), billing.PSPImportListParams{Origin: billing.PSPImportOnboarding, PageRequest: billing.PageRequest{Limit: 100}})
			if err != nil {
				return err
			}
			byPSP := map[billing.PSPID]int{}
			jobs, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import").First(1000))
			if err != nil {
				return err
			}
			for _, imp := range page.Items {
				byPSP[imp.PSP]++
				n := 0
				for _, job := range jobs.Jobs {
					if strings.Contains(string(job.EncodedArgs), imp.ID.UUID().String()) {
						n++
					}
				}
				if n != 1 {
					return fmt.Errorf("import %s has %d jobs", imp.ID, n)
				}
			}
			if len(byPSP) != 3 || len(page.Items) != 3 {
				return fmt.Errorf("%d onboarding imports of %d PSPs", len(page.Items), len(byPSP))
			}
			return nil
		}
		// boot starts n more processes at once on the database, as Doujins and
		// Hentai0 serve one merchant, and stops them once each loaded it.
		boot := func(n int) {
			engines := make([]*openrails.Engine, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() {
					engines[i], errs[i] = openrails.New(t.Context(), iw.cfg, iw.deps)
					if errs[i] == nil {
						client, err := engines[i].MerchantBySlug("alpha")
						if err == nil {
							_, err = client.ListPSPs(t.Context(), billing.PSPListParams{})
						}
						errs[i] = err
					}
				})
			}
			wg.Wait()
			require.NoError(t, errors.Join(errs...))
			require.Eventually(t, func() bool { return state() == nil }, 20*time.Second, 20*time.Millisecond, "%v", state())
			require.Never(t, func() bool { return state() != nil }, 2*time.Second, 100*time.Millisecond)
			for _, e := range engines {
				require.NoError(t, e.Close(context.Background()))
			}
		}
		boot(3)
		boot(2)
		require.NoError(t, state(), "restarting creates nothing more")
		iw.restart(t, nil)
		for _, key := range []string{"nmi", "stripe", "ccbill"} {
			iw.await(t, iw.onboarding(t, key).ID, billing.PSPImportPulled)
		}
		require.NoError(t, state())
	})

	t.Run("OR-J38-S08/credentials-rejected-rearms", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi", "stripe", "ccbill")
		refuseNMI(iw.nmi, "critical-import-key")
		iw.stripe.Intercept(func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer sk_test_import" },
			func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
				return answer(r, http.StatusUnauthorized, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided"}}`), nil
			})
		iw.ccbill.RequireCredentials("critical-datalink", "rotated-datalink-password", false)
		iw.start(t)

		keys := []string{"nmi", "stripe", "ccbill"}
		failed := map[string]*billing.PSPImport{}
		for _, key := range keys {
			failed[key] = iw.await(t, iw.onboarding(t, key).ID, billing.PSPImportFailed)
			require.Equal(t, billing.PSPImportCredentialsRejected, *failed[key].ErrorCode, "%s: %v", key, failed[key].Error)
		}
		findings := func(status billing.FindingStatus) map[billing.PSPID]billing.Finding {
			t.Helper()
			page, err := iw.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.psp_import.credentials_rejected", Status: status, PageRequest: billing.PageRequest{Limit: 100}})
			require.NoError(t, err)
			out := map[billing.PSPID]billing.Finding{}
			for _, f := range page.Items {
				psp, err := billing.ParsePSPID(f.SubjectKey)
				require.NoError(t, err)
				require.NotContains(t, out, psp, "one finding per PSP")
				out[psp] = f
			}
			return out
		}
		open := findings("")
		require.Len(t, open, 3)
		for _, key := range keys {
			f := open[failed[key].PSP]
			require.Equal(t, billing.FindingStatus("requires_review"), f.Status, key)
			require.Equal(t, "high", f.Severity)
		}

		// The same credentials, loaded by a new process: each import restarts in
		// place and fails again.
		iw.restart(t, nil)
		for _, key := range keys {
			id := failed[key].ID
			require.Eventually(t, func() bool {
				jobs, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import").States(rivertype.JobStateCancelled).First(1000))
				if err != nil {
					return false
				}
				n := 0
				for _, job := range jobs.Jobs {
					if strings.Contains(string(job.EncodedArgs), id.UUID().String()) {
						n++
					}
				}
				return n == 2
			}, 30*time.Second, 50*time.Millisecond, "%s re-armed and failed again", key)
			again := iw.await(t, id, billing.PSPImportFailed)
			require.Equal(t, billing.PSPImportCredentialsRejected, *again.ErrorCode)
			require.Equal(t, id, iw.onboarding(t, key).ID, "re-arming reuses the import")
		}
		open = findings("")
		require.Len(t, open, 3)
		for _, f := range open {
			require.Equal(t, billing.FindingStatus("requires_review"), f.Status)
		}

		// Replaced credentials pull, and the findings close.
		iw.restart(t, func(psps openrails.PSPs) {
			psps["nmi"] = importNMIPSP("critical-import-gateway", "critical-import-key-rotated", false)
			psps["stripe"] = importStripePSP("sk_test_import_rotated")
			psps["ccbill"] = importCCBillPSP("0000", "rotated-datalink-password")
		})
		for _, key := range keys {
			id := failed[key].ID
			require.Eventually(t, func() bool {
				imp, err := iw.api.GetPSPImport(t.Context(), id)
				return err == nil && imp.Status == billing.PSPImportPulled
			}, 30*time.Second, 50*time.Millisecond, "%s restarts and pulls", key)
			pulled := iw.await(t, id, billing.PSPImportPulled)
			require.Nil(t, pulled.ErrorCode)
			require.Nil(t, pulled.Error)
		}
		require.Empty(t, findings(""))
		closed := findings("fixed")
		require.Len(t, closed, 3)
		for _, f := range closed {
			require.Equal(t, billing.FindingStatus("fixed"), f.Status)
			require.NotNil(t, f.ResolvedAt)
		}
		require.Len(t, iw.onboardings(t), 3)
	})

	t.Run("OR-J38-S09/provider-unreachable-retries", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi", "stripe")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil), "pm_bob", "price_basic", 999, start, start.AddDate(0, 1, 0))
		iw.clock.Advance(time.Minute)
		iw.nmi.QueryUnavailable(true)
		iw.stripe.ChargeListUnavailable(true)
		iw.start(t)

		imports := map[string]billing.PSPImportID{"nmi": iw.onboarding(t, "nmi").ID, "stripe": iw.onboarding(t, "stripe").ID}
		// failing is the import's job once an attempt numbered at least from
		// has failed and waits to retry.
		failing := func(id billing.PSPImportID, from int) *rivertype.JobRow {
			t.Helper()
			var job *rivertype.JobRow
			require.Eventually(t, func() bool {
				jobs, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import").First(1000))
				if err != nil {
					return false
				}
				for _, j := range jobs.Jobs {
					if strings.Contains(string(j.EncodedArgs), id.UUID().String()) && j.State == rivertype.JobStateRetryable && j.Attempt >= from {
						job = j
					}
				}
				return job != nil
			}, 30*time.Second, 20*time.Millisecond, "attempt %d of %s failed and waits to retry", from, id)
			return job
		}
		// Past the five attempts that used to end an import: each retry is
		// driven at once instead of waiting attempt⁴ seconds.
		const attempts = 8
		for key, id := range imports {
			for from := 1; ; {
				job := failing(id, from)
				require.Equal(t, 10_000, job.MaxAttempts)
				imp, err := iw.api.GetPSPImport(t.Context(), id)
				require.NoError(t, err)
				require.Equal(t, billing.PSPImportPulling, imp.Status, "%s attempt %d: an unreachable provider never fails the import", key, job.Attempt)
				require.Nil(t, imp.Error)
				if job.Attempt >= attempts {
					require.WithinDuration(t, time.Now().Add(time.Hour), job.ScheduledAt, 5*time.Minute, "%d⁴ s waits at most an hour", job.Attempt)
					break
				}
				_, err = iw.fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
				from = job.Attempt + 1
			}
		}
		rosters, customers := iw.nmi.Reads()["v5:customers"], iw.stripe.Reads("/v1/customers")
		require.Equal(t, 1, rosters)
		iw.nmi.QueryUnavailable(false)
		iw.stripe.ChargeListUnavailable(false)
		for key, id := range imports {
			_, err := iw.fleet.JobRetry(t.Context(), failing(id, attempts).ID)
			require.NoError(t, err)
			imp := iw.await(t, id, billing.PSPImportPulled)
			require.Equal(t, imp.PulledAt, imp.HistoryThrough, key)
			var sales int
			for _, r := range iw.records(t, id) {
				if r.Kind == billing.PSPImportTransaction {
					sales++
				}
			}
			require.Equal(t, 1, sales, "%s: the history read once the provider answers", key)
		}
		require.Equal(t, rosters, iw.nmi.Reads()["v5:customers"], "the roster is not read again")
		require.Equal(t, customers, iw.stripe.Reads("/v1/customers"))
	})

	t.Run("OR-J38-S10/apply-lands-once-per-rail", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi", "stripe", "ccbill")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.bob.Email, "Bob", nil), "pm_bob", "price_basic", 999, start, start.AddDate(0, 1, 0))
		carol := authtest.NewUser(t, iw.auth)
		next := iw.clock.Now().Add(20 * 24 * time.Hour)
		member := iw.ccbill.AddMember(ccbillmock.Member{Username: carol.Username, Email: carol.Email, Signup: start, Rebill: &next})
		iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: member, At: start.Add(time.Hour), Amount: "23.00"})
		iw.clock.Advance(time.Minute)
		iw.start(t)
		keys := []string{"nmi", "stripe", "ccbill"}
		imports := map[string]billing.PSPImportID{}
		for _, key := range keys {
			imports[key] = iw.await(t, iw.onboarding(t, key).ID, billing.PSPImportPulled).ID
		}

		for _, key := range []string{"gold", "basic", "premium"} {
			_, err := iw.api.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
		}
		for _, p := range []billing.CreateProviderPriceParams{
			{ProductKey: "gold", Key: "nmi-gold", PSP: "nmi", PlanID: "gold", Archived: true},
			{ProductKey: "basic", Key: "stripe-basic", PSP: "stripe", PriceID: "price_basic", Archived: true},
			{ProductKey: "premium", Key: "ccbill-23", PSP: "ccbill", FlexID: "681cb38f-afb9-4665-931f-2b896072178a", RecurringBillingOptionID: "0000000023",
				UnitAmount: ptr(int64(23_000_000)), Currency: "USD", BillingIntervalHours: ptr(720), Archived: true},
		} {
			_, err := iw.api.CreateProviderPrice(t.Context(), p)
			require.NoError(t, err)
		}
		type landed struct{ subscriptions, payments, methods int }
		members := map[string]billing.CustomerID{"nmi": customerOf(iw.alice), "stripe": customerOf(iw.bob), "ccbill": customerOf(carol)}
		book := func() map[string]landed {
			t.Helper()
			out := map[string]landed{}
			for key, who := range members {
				payments, err := iw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: who})
				require.NoError(t, err)
				methods, err := iw.alpha.ListPaymentMethods(t.Context(), who, billing.PaymentMethodListParams{})
				require.NoError(t, err)
				out[key] = landed{len(iw.subscriptions(t, who)), len(payments.Items), len(methods.Items)}
			}
			return out
		}
		applyAll := func(ids map[string]billing.PSPImportID) {
			t.Helper()
			for key, id := range ids {
				_, err := iw.api.LinkPSPImport(t.Context(), id, billing.LinkPSPImportParams{})
				require.NoError(t, err)
				for ref, r := range iw.apply(t, id) {
					require.Equal(t, billing.PSPImportImported, r.State, "%s %s %s: %v", key, r.Kind, ref, r.Reason)
				}
			}
		}
		applyAll(imports)
		want := map[string]landed{"nmi": {1, 1, 1}, "stripe": {1, 1, 1}, "ccbill": {1, 1, 0}}
		require.Equal(t, want, book())
		methods, err := iw.alpha.ListPaymentMethods(t.Context(), customerOf(iw.alice), billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Equal(t, "nmi", methods.Items[0].Rail, "the NMI vault lands as the customer's card")
		require.Equal(t, "4242", *methods.Items[0].Card.Last4)
		for key, entitlement := range map[string]string{"nmi": "member:gold", "stripe": "member:basic", "ccbill": "member:premium"} {
			require.True(t, iw.entitled(t, members[key], entitlement), key)
		}

		applyAll(imports)
		require.Equal(t, want, book(), "relinking and applying again lands nothing twice")
		manual := map[string]billing.PSPImportID{}
		for _, key := range keys {
			manual[key] = iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: key}}).ID
		}
		applyAll(manual)
		require.Equal(t, want, book(), "a later manual import of the same PSP finds everything landed")
	})

	t.Run("OR-J38-S11/stripe-customer-attribution", func(t *testing.T) {
		iw := bootImportWorld(t, "stripe")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
		for _, p := range []stripemock.Price{{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"}, {ID: "price_extra", Product: "prod_extra", Amount: 500, Interval: "month"}} {
			iw.stripe.AddProduct(p.Product, p.Product)
			iw.stripe.AddPrice(p)
		}
		carol, dave := authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth)
		customers := map[string]string{}
		for name, u := range map[string]authtest.User{"alice": iw.alice, "bob": iw.bob, "carol": carol} {
			customers[name] = iw.stripe.AddCustomer(u.Email, name, nil)
			iw.stripe.AddSubscription(customers[name], "pm_"+name, "price_basic", 999, start, start.AddDate(0, 1, 0))
		}
		// Bob already maps another Stripe customer (made by a checkout); Carol's
		// belongs to Dave.
		pspID := iw.psp(t, "stripe")
		held, err := iw.api.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: iw.clock.Now(), DefaultPSP: billing.PSPRef{ID: &pspID},
			Customers: []billing.DeclaredCustomer{{Customer: customerOf(iw.bob)}, {Customer: customerOf(dave)}},
			PSPCustomers: []billing.DeclaredPSPCustomer{{SourceID: "bob", Customer: customerOf(iw.bob), Rail: "stripe", RemoteCustomerRef: "cus_from_checkout"},
				{SourceID: "dave", Customer: customerOf(dave), Rail: "stripe", RemoteCustomerRef: customers["carol"]}}})
		require.NoError(t, err)
		require.Empty(t, held.Reasons)
		iw.clock.Advance(time.Minute)
		iw.start(t)
		imp := iw.await(t, iw.onboarding(t, "stripe").ID, billing.PSPImportPulled)
		for _, p := range []billing.CreateProviderPriceParams{{Key: "basic", PSP: "stripe", PriceID: "price_basic"}, {Key: "extra", PSP: "stripe", PriceID: "price_extra"}} {
			_, err := iw.api.CreateProduct(t.Context(), billing.CreateProductParams{Key: p.Key, DisplayName: p.Key, Entitlements: []string{"member:" + p.Key}})
			require.NoError(t, err)
			p.ProductKey, p.Key = p.Key, "stripe-"+p.Key
			_, err = iw.api.CreateProviderPrice(t.Context(), p)
			require.NoError(t, err)
		}
		_, err = iw.api.LinkPSPImport(t.Context(), imp.ID, billing.LinkPSPImportParams{})
		require.NoError(t, err)
		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[customers["alice"]].State)
		require.Nil(t, recs[customers["alice"]].Reason)
		require.Equal(t, billing.PSPImportImported, recs[customers["bob"]].State, "a customer already mapping another keeps it")
		require.Contains(t, *recs[customers["bob"]].Reason, "cus_from_checkout")
		require.Equal(t, billing.PSPImportBlocked, recs[customers["carol"]].State, "a customer object another customer holds blocks only its customer")
		require.Contains(t, *recs[customers["carol"]].Reason, "already belongs")
		require.Len(t, iw.subscriptions(t, customerOf(iw.alice)), 1)
		require.Len(t, iw.subscriptions(t, customerOf(iw.bob)), 1)
		require.Empty(t, iw.subscriptions(t, customerOf(carol)))

		// Stripe bills Alice's customer for a subscription OpenRails never
		// sold: no user id names her; the landed link does.
		oob := iw.stripe.AddSubscription(customers["alice"], "pm_alice_extra", "price_extra", 500, iw.clock.Now().Add(-time.Hour), iw.clock.Now().AddDate(0, 1, 0))
		body := stripeNotice(t, "evt_out_of_band", "customer.subscription.updated", stripemock.Object{"id": oob, "object": "subscription"})
		status, raw := callback(t, iw.authorityWorld, "acct_import", body, stripeSignature(body, "whsec_import"))
		require.Equal(t, http.StatusOK, status, raw)
		require.Eventually(t, func() bool {
			subs, err := iw.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customerOf(iw.alice)})
			return err == nil && len(subs.Items) == 2
		}, 30*time.Second, 50*time.Millisecond, "the out-of-band subscription is Alice's")
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:extra"))
	})

	t.Run("OR-J38-S12/psp-added-and-rotated-at-runtime", func(t *testing.T) {
		a, p := newPSPWorld(t)
		iw := &importWorld{authorityWorld: a, api: a.alphaRemote, fleet: startFleet(t, a.world)}
		// A restricted key the provider lets probe its account but not read
		// the catalog.
		p.provider.core.Intercept(func(r *http.Request) bool { return r.URL.Path == "/v1/products" }, func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
			return answer(r, http.StatusForbidden, `{"error":{"type":"invalid_request_error","message":"The provided key does not have the required permissions"}}`), nil
		})
		psp, err := a.alphaRemote.CreatePSP(t.Context(), pspInput())
		require.NoError(t, err)
		imp := iw.await(t, iw.onboarding(t, "stripe").ID, billing.PSPImportFailed)
		require.Equal(t, billing.PSPImportCredentialsRejected, *imp.ErrorCode, "a PSP added to a running process gets its pull")
		findings := func(status billing.FindingStatus) []billing.Finding {
			t.Helper()
			page, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.psp_import.credentials_rejected", Status: status})
			require.NoError(t, err)
			return page.Items
		}
		require.Len(t, findings(""), 1)

		// The merchant rotates to a key that reads the catalog: the running
		// process restarts the import in place.
		p.provider.core.ClearIntercepts()
		_, err = a.alphaRemote.UpdatePSP(t.Context(), psp.ID, billing.UpdatePSPParams{ExpectedRevision: &psp.Revision, Credentials: map[string]string{"secret_key": "sk_test_rotated"}})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			got, err := a.alphaRemote.GetPSPImport(t.Context(), imp.ID)
			return err == nil && got.Status == billing.PSPImportPulled
		}, 30*time.Second, 50*time.Millisecond, "the rotated credentials pull")
		require.Equal(t, imp.ID, iw.onboarding(t, "stripe").ID)
		require.Empty(t, findings(""))
		require.Len(t, findings("fixed"), 1)
		require.Empty(t, p.provider.effects(t), "the pull moves no money")
	})

	t.Run("OR-J38-S13/unreadable-answer-fails-visibly", func(t *testing.T) {
		iw := bootImportWorld(t, "stripe")
		// Stripe answers a customer list OpenRails cannot read: no later
		// attempt reads it either.
		iw.stripe.Intercept(func(r *http.Request) bool { return r.Method == http.MethodGet && r.URL.Path == "/v1/customers" },
			func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
				return answer(r, http.StatusOK, `{"object":"list","url":"/v1/customers","has_more":false,"data":[{"object":"customer","email":"nobody@example.com"}]}`), nil
			})
		iw.start(t)
		id := iw.onboarding(t, "stripe").ID
		var last *rivertype.JobRow
		require.Eventually(t, func() bool {
			imp, err := iw.api.GetPSPImport(t.Context(), id)
			if err != nil {
				return false
			}
			if imp.Status == billing.PSPImportFailed {
				return true
			}
			jobs, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import").First(1000))
			if err != nil {
				return false
			}
			for _, j := range jobs.Jobs {
				if strings.Contains(string(j.EncodedArgs), id.UUID().String()) && j.State == rivertype.JobStateRetryable {
					last = j
					_, _ = iw.fleet.JobRetry(t.Context(), j.ID)
				}
			}
			return false
		}, 60*time.Second, 50*time.Millisecond, "the import fails")
		require.NotNil(t, last)
		imp := iw.await(t, id, billing.PSPImportFailed)
		require.Contains(t, *imp.Error, "stripe customer is unreadable")
		require.Nil(t, imp.ErrorCode, "no configuration change fixes it")
		var job *rivertype.JobRow
		require.Eventually(t, func() bool {
			got, err := iw.fleet.JobGet(t.Context(), last.ID)
			job = got
			return err == nil && got.State == rivertype.JobStateCancelled
		}, 20*time.Second, 20*time.Millisecond, "the job ends with the import")
		require.Equal(t, 5, job.Attempt, "a failure no attempt outlives is tried 5 times")
		require.Len(t, iw.jobs(t, id), 1)
		require.NoError(t, iw.api.DeletePSPImport(t.Context(), id), "a failed import can be deleted")
	})
}
