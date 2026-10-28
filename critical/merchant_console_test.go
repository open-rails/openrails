//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// consoleAction is a step the spec asks of the fixture between browser steps.
// It runs on a request goroutine, so it fails by returning an error.
type consoleAction func(ctx context.Context) (any, error)

// consoleStaff signs in a new person holding exactly perms in alpha's group,
// through a custom role named name.
func consoleStaff(t *testing.T, a *authorityWorld, name string, perms ...billing.Permission) string {
	t.Helper()
	user := authtest.NewUser(t, a.auth)
	granted := make([]iam.Perm, 0, len(perms))
	for _, p := range perms {
		granted = append(granted, mustPermission(t, string(p)))
	}
	role, err := a.auth.CreateGroupRole(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewGroupRole{Name: name, Permissions: granted})
	require.NoError(t, err)
	_, err = a.auth.SetGroupRole(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.UserSubject(user.ID), role.Name)
	require.NoError(t, err)
	return authtest.SignIn(t, a.auth, user).AccessToken
}

// consoleOperator holds what a person reviewing an import needs: merchant
// configuration, the catalog to map prices, and customer support to see
// whom a record links.
func consoleOperator(t *testing.T, a *authorityWorld) string {
	t.Helper()
	return consoleStaff(t, a, "console-operator", billing.MerchantConfigRead, billing.MerchantConfigManage,
		billing.MerchantCatalogRead, billing.MerchantCatalogManage, billing.MerchantCustomerRead)
}

// consoleBrowser drives the built BillingAdmin in Chromium. This server only
// serves the console and the fixture; every API request is proxied to the
// real router and counted by method and path, which it returns.
func consoleBrowser(t *testing.T, a *authorityWorld, config map[string]any, actions map[string]consoleAction, cases ...string) map[string]int {
	t.Helper()
	directory, err := filepath.Abs("../sdk/billing-ui")
	require.NoError(t, err)
	app := filepath.Join(directory, "test-results", "admin-app")
	_, err = os.Stat(filepath.Join(app, "app.js"))
	require.NoError(t, err, "critical:build must build the console browser bundle")
	target, err := url.Parse(a.server.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	counts := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__fixture/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(config)
	})
	mux.HandleFunc("POST /__fixture/{action}", func(w http.ResponseWriter, r *http.Request) {
		action, ok := actions[r.PathValue("action")]
		if !ok {
			http.Error(w, "no such fixture action", http.StatusNotFound)
			return
		}
		out, err := action(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /__admin/admin.css", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(directory, "dist", "admin.css"))
	})
	mux.Handle("GET /__admin/", http.StripPrefix("/__admin/", http.FileServer(http.Dir(app))))
	mux.HandleFunc("GET /console/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><html><head><link rel="stylesheet" href="/__admin/admin.css"></head><body><div id="app"></div><script type="module" src="/__admin/app.js"></script></body></html>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	results := t.TempDir()
	if root := os.Getenv("OPENRAILS_CRITICAL_REPORT_DIR"); root != "" {
		root, err = filepath.Abs(filepath.Join(root, "browser"))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(root, 0700))
		results, err = os.MkdirTemp(root, strings.Join(cases, "_")+"-")
		require.NoError(t, err)
	}
	report := filepath.Join(results, "report.json")
	command := exec.CommandContext(t.Context(), "pnpm", "exec", "playwright", "test", "-c", "playwright.admin.config.ts", "--grep", strings.Join(cases, "|"))
	command.Dir = directory
	command.Env = append(os.Environ(), "OPENRAILS_BROWSER_URL="+server.URL, "OPENRAILS_BROWSER_RESULTS="+results, "OPENRAILS_BROWSER_REPORT="+report)
	output, err := command.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err, "built console browser assertions failed")
	raw, err := os.ReadFile(report)
	require.NoError(t, err)
	var receipt struct {
		Stats struct{ Expected, Unexpected, Flaky, Skipped int }
	}
	require.NoError(t, json.Unmarshal(raw, &receipt))
	require.Equal(t, len(cases), receipt.Stats.Expected, "every selected console case ran and passed")
	require.Zero(t, receipt.Stats.Unexpected)
	require.Zero(t, receipt.Stats.Flaky)
	require.Zero(t, receipt.Stats.Skipped)
	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(counts)
}

// consoleStart is the world's start as a fixture action.
func consoleStart(t *testing.T, iw *importWorld) consoleAction {
	return func(context.Context) (any, error) {
		if err := iw.engine.Start(t.Context(), openrails.WithRiverClient(iw.fleet)); err != nil {
			return nil, err
		}
		return nil, iw.fleet.Start(t.Context())
	}
}

// settleJobs runs every queued job of kinds now, instead of after its
// debounce, until none is left to run.
func settleJobs(ctx context.Context, fleet *river.Client[pgx.Tx], kinds ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		res, err := fleet.JobList(ctx, river.NewJobListParams().Kinds(kinds...).First(200))
		if err != nil {
			return err
		}
		idle := true
		for _, j := range res.Jobs {
			switch j.State {
			case rivertype.JobStateScheduled, rivertype.JobStateRetryable:
				if _, err := fleet.JobRetry(ctx, j.ID); err != nil {
					return err
				}
				idle = false
			case rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStatePending:
				idle = false
			}
		}
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("jobs did not settle")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// writes are the requests that were not reads: a metrics query is a read.
func writes(counts map[string]int) map[string]int {
	out := map[string]int{}
	for k, n := range counts {
		if !strings.HasPrefix(k, "GET ") && k != "POST /v1/metrics/query" {
			out[k] = n
		}
	}
	return out
}

func TestMerchantConsole(t *testing.T) {
	t.Run("OR-J38-S30/console-pull-link-map-apply-lands-once", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "silver", Name: "Silver", Amount: "10.00", Days: 30})
		carol := authtest.NewUser(t, iw.auth)
		alice := iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		bob := iw.nmiMember(t, iw.bob.Email, "silver", "10.00", start)
		vault := iw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1881"})
		iw.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Email = "carol@legacy.invalid" })
		legacy := iw.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "gold", Amount: "20.00", Days: 30, Created: start, NextBilling: start.AddDate(0, 0, 30), Order: "order-legacy"})
		iw.nmi.AddScheduleSale(legacy, start)
		products := map[string]billing.ProductID{}
		for key, name := range map[string]string{"gold": "Gold", "silver": "Silver"} {
			product, err := iw.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: name, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
			products[key] = product.ID
		}
		gold, err := iw.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: products["gold"], Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)
		imp := iw.onboarding(t, "nmi")
		require.Equal(t, billing.PSPImportPulling, imp.Status, "the fleet has not run its pull")

		counts := consoleBrowser(t, iw.authorityWorld,
			map[string]any{"tokens": map[string]string{"staff": consoleOperator(t, iw.authorityWorld)}, "carol": carol.ID, "vault": vault},
			map[string]consoleAction{"start": consoleStart(t, iw)},
			"console-pull-link-map-apply-lands-once")

		var silver *billing.Price
		prices, err := iw.alpha.ListPrices(t.Context(), billing.PriceListParams{})
		require.NoError(t, err)
		for i, p := range prices.Items {
			if p.Key == "nmi-silver" {
				silver = &prices.Items[i]
			}
		}
		require.NotNil(t, silver, "Map to product created the provider price under its default key")
		require.True(t, silver.Archived, "mapped from an import, it is no tier-change target by default")
		require.Equal(t, products["silver"], silver.ProductID)
		require.EqualValues(t, 10_000_000, silver.UnitAmount)
		require.Equal(t, "USD", silver.Currency)
		all, err := iw.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{})
		require.NoError(t, err)
		require.Len(t, all.Items, 3, "Apply and Apply again land each subscription once")
		for _, want := range []struct {
			customer billing.CustomerID
			ref      string
			price    billing.PriceID
		}{{customerOf(iw.alice), alice, gold.ID}, {customerOf(iw.bob), bob, silver.ID}, {billing.CustomerID(uuid.MustParse(carol.ID)), legacy, gold.ID}} {
			subs := iw.subscriptions(t, want.customer)
			require.Len(t, subs, 1)
			require.Equal(t, want.ref, *subs[0].RailSubscriptionID)
			require.Equal(t, want.price, subs[0].PriceID)
			require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		}
		require.True(t, iw.entitled(t, billing.CustomerID(uuid.MustParse(carol.ID)), "member:gold"))
		records := iw.records(t, imp.ID)
		require.Equal(t, "manual", *records[vault].LinkedVia)
		require.Equal(t, billing.PSPImportImported, records[legacy].State)
		require.Len(t, iw.nmi.Sales(), 3, "the seeded sales only: nothing was charged")
		require.Empty(t, iw.nmi.Calls())
		require.Empty(t, iw.nmi.Unexpected())
		require.Equal(t, 2, counts["POST /v1/merchant/psp-imports/"+imp.ID.String()+"/apply"], "Apply and Apply again")
		require.Equal(t, 1, counts["POST /v1/merchant/psp-imports/"+imp.ID.String()+"/link"], "the manual link")
		require.Equal(t, 1, counts["POST /v1/catalog/provider-prices"])
	})

	t.Run("OR-J38-S31/console-mapping-upload-and-ccbill-price", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		flex := "681cb38f-afb9-4665-931f-2b896072178a"
		price := func(product, key, rbo string, amount int64, archived bool) *billing.Price {
			t.Helper()
			p, err := iw.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductKey: product, Key: key, PSP: "ccbill", FlexID: flex, RecurringBillingOptionID: rbo,
				UnitAmount: &amount, Currency: "USD", BillingIntervalHours: new(720), Archived: archived})
			require.NoError(t, err)
			return p
		}
		for _, key := range []string{"premium", "basic"} {
			_, err := iw.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
		}
		premium := price("premium", "ccbill-23", "0000000023", 23_000_000, false)
		picked := price("basic", "ccbill-999-a", "0000000999", 9_990_000, false)
		price("basic", "ccbill-999-b", "0000001999", 9_990_000, true)
		carol, dave := authtest.NewUser(t, iw.auth), authtest.NewUser(t, iw.auth)
		signup := iw.clock.Now().Add(-40 * 24 * time.Hour)
		rebilled := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour).Add(18 * time.Hour)
		next := rebilled.Add(30 * 24 * time.Hour)
		// The legacy site's emails are not the directory's: only the
		// merchant's username mapping links these members.
		member := func(u authtest.User, rebill string) string {
			id := iw.ccbill.AddMember(ccbillmock.Member{Username: u.Username, Email: u.Username + "@legacy.invalid", Signup: signup, Rebill: &next})
			iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: id, At: rebilled, Amount: rebill})
			return id
		}
		m23, m999, unmapped := member(iw.alice, "23.00"), member(carol, "9.99"), member(dave, "23.00")
		iw.start(t)
		imp := iw.await(t, iw.onboarding(t, "ccbill").ID, billing.PSPImportPulled)
		_, err := iw.api.UpdatePSPImport(t.Context(), imp.ID, billing.UpdatePSPImportParams{Rules: &[]billing.PSPImportRule{{Fact: "username", Via: billing.PSPImportViaMapping, Mapping: "legacy"}}})
		require.NoError(t, err)
		_, err = iw.api.SetPSPImportMappings(t.Context(), billing.SetPSPImportMappingsParams{Mappings: []billing.PSPImportMapping{{Mapping: "legacy", Key: dave.Username, Customer: new(billing.CustomerID(uuid.MustParse(dave.ID)))}}})
		require.NoError(t, err)
		csv := []string{"mapping,key,customer", "legacy," + iw.alice.Username + "," + iw.alice.ID, "legacy," + carol.Username + "," + carol.ID}
		for i := range 147 {
			csv = append(csv, fmt.Sprintf("legacy,former-member-%03d,%s", i, uuid.NewString()))
		}
		csv = append(csv, "legacy,"+dave.Username+",")

		counts := consoleBrowser(t, iw.authorityWorld,
			map[string]any{"tokens": map[string]string{"staff": consoleOperator(t, iw.authorityWorld)}, "csv": strings.Join(csv, "\r\n") + "\r\n", "blocked": m999, "picked": picked.ID.String()},
			nil, "console-mapping-upload-and-ccbill-price")

		require.Equal(t, 2, counts["POST /v1/merchant/psp-import-mappings"], "150 rows go out 100 a request")
		records := iw.records(t, imp.ID)
		require.Equal(t, "rule:1", *records[m23].LinkedVia)
		require.Equal(t, billing.PSPImportImported, records[m23].State)
		require.Equal(t, billing.PSPImportUnmatched, records[unmapped].State, "the upload deleted its mapping")
		require.Equal(t, picked.ID, *records[m999].ManualPrice)
		require.Equal(t, billing.PSPImportImported, records[m999].State)
		subs := iw.subscriptions(t, customerOf(iw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, premium.ID, subs[0].PriceID)
		subs = iw.subscriptions(t, billing.CustomerID(uuid.MustParse(carol.ID)))
		require.Len(t, subs, 1)
		require.Equal(t, picked.ID, subs[0].PriceID, "the price picked by hand")
		require.Empty(t, iw.subscriptions(t, billing.CustomerID(uuid.MustParse(dave.ID))))
	})

	t.Run("OR-J38-S32/console-proposals-group-approve-held-dismiss", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := openPeriod(iw)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.premium(t, "gold")
		carol := authtest.NewUser(t, iw.auth)
		renewed := []string{iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start), iw.nmiMember(t, carol.Email, "gold", "20.00", start)}
		gone := iw.nmiMember(t, iw.bob.Email, "gold", "20.00", start)
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		iw.apply(t, first.ID)
		// NMI billed two renewals OpenRails has not seen, and deleted a schedule.
		for _, ref := range renewed {
			iw.renewEarly(ref)
		}
		iw.nmi.DeleteSchedule(gone)

		counts := consoleBrowser(t, iw.authorityWorld, map[string]any{"tokens": map[string]string{"staff": consoleOperator(t, iw.authorityWorld)}, "first": first.ID.String()},
			nil, "console-proposals-group-approve-held-dismiss")
		for _, step := range []string{"link", "apply"} {
			require.Zero(t, counts["POST /v1/merchant/psp-imports/"+first.ID.String()+"/"+step], "the console offers no %s of a superseded import", step)
		}

		imports, err := iw.alpha.ListPSPImports(t.Context(), billing.PSPImportListParams{Origin: billing.PSPImportManual})
		require.NoError(t, err)
		reconciled := imports.Items[0]
		require.Equal(t, billing.PSPImportApplied, reconciled.Status)
		require.Equal(t, billing.PSPImportProposals{Approved: 2, Dismissed: 1, Superseded: 2}, reconciled.Proposals)
		page, err := iw.alpha.ListProposals(t.Context(), billing.ProposalListParams{Pull: reconciled.ID, Status: "fixed", PageRequest: billing.PageRequest{Limit: 100}})
		require.NoError(t, err)
		var run *billing.RunID
		approved := 0
		for _, f := range page.Items {
			if f.Resolution == nil || *f.Resolution != "approved" {
				continue
			}
			approved++
			require.Equal(t, billing.ChangeRecordCharge, f.Change.Kind)
			if run == nil {
				run = f.ApprovalRunID
			}
			require.Equal(t, *run, *f.ApprovalRunID, "Apply approved its corrections under one run")
		}
		require.Equal(t, 2, approved)
		for _, customer := range []billing.CustomerID{customerOf(iw.alice), billing.CustomerID(uuid.MustParse(carol.ID))} {
			require.Equal(t, []int64{20_000_000, 20_000_000}, paidAmounts(t, &trailWorld{iw}, customer), "the missed renewal is recorded once, exactly")
		}
		bob := iw.subscriptions(t, customerOf(iw.bob))
		require.Len(t, bob, 1)
		require.Equal(t, billing.SubscriptionActive, bob[0].Status, "held, then dismissed: access stays")
		require.True(t, iw.entitled(t, customerOf(iw.bob), "member:premium"))
		require.Empty(t, iw.nmi.Calls())
	})

	t.Run("OR-J38-S33/console-parked-signup-lands-after-mapping", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_gold", "Gold")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_gold", Product: "prod_gold", Amount: 1500, Interval: "month"})
		start := tw.clock.Now().Add(-time.Hour)
		customer := tw.stripe.AddCustomer(tw.alice.Email, "Alice", nil)
		sub := tw.stripe.AddSubscription(customer, "pm_alice", "price_gold", 1500, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(sub, "user_id", tw.alice.ID, start)
		tw.invoicePaid(t, sub)
		completedOnce(t, tw.fleet, kindConverge)
		require.Equal(t, billing.ParkPriceUnmapped, openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), sub).Reason)
		finding := tw.findings(t, "catalog.price.unmapped")["price_gold"]
		require.Nil(t, finding.ResolvedAt)
		product := tw.stripeProduct(t, "gold", "member:gold")

		consoleBrowser(t, tw.authorityWorld,
			map[string]any{"tokens": map[string]string{"staff": consoleOperator(t, tw.authorityWorld)}, "subscription": sub, "finding": finding.ID.String()},
			map[string]consoleAction{"settle": func(ctx context.Context) (any, error) { return nil, settleJobs(ctx, tw.fleet, kindConverge) }},
			"console-parked-signup-lands-after-mapping")

		subs := tw.subscriptions(t, customerOf(tw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, sub, *subs[0].RailSubscriptionID)
		price, err := tw.alpha.GetPrice(t.Context(), subs[0].PriceID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.Equal(t, product, price.ProductID)
		require.True(t, price.Archived)
		require.True(t, tw.entitled(t, customerOf(tw.alice), "member:gold"))
		closed := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), sub)
		require.NotNil(t, closed.ReleasedAt)
		require.Equal(t, billing.ParkLanded, *closed.ReleasedBy)
		require.Equal(t, subs[0].ID, *closed.Subscription)
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}))
		require.NotNil(t, tw.findings(t, "catalog.price.unmapped")["price_gold"].ResolvedAt)
	})

	t.Run("OR-J38-S34/console-failed-pull-and-read-only-access", func(t *testing.T) {
		iw := bootImportWorld(t, "nmi", "stripe")
		iw.stripe.Intercept(func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer sk_test_import" },
			func(r *http.Request, _ func() *http.Response) (*http.Response, error) {
				return answer(r, http.StatusUnauthorized, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided"}}`), nil
			})
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		iw.start(t)
		nmiImport := iw.await(t, iw.onboarding(t, "nmi").ID, billing.PSPImportPulled)
		stripeImport := iw.await(t, iw.onboarding(t, "stripe").ID, billing.PSPImportFailed)
		require.Equal(t, billing.PSPImportCredentialsRejected, *stripeImport.ErrorCode)

		counts := consoleBrowser(t, iw.authorityWorld,
			map[string]any{
				"tokens": map[string]string{
					"reader": consoleStaff(t, iw.authorityWorld, "console-reader", billing.MerchantConfigRead),
					"config": consoleStaff(t, iw.authorityWorld, "console-config", billing.MerchantConfigRead, billing.MerchantConfigManage),
				},
				"nmiImport": nmiImport.ID.String(), "stripeImport": stripeImport.ID.String(),
			},
			nil, "console-failed-pull-and-read-only-access")

		require.Empty(t, writes(counts), "reading every page writes nothing")
		require.Empty(t, iw.records(t, stripeImport.ID), "a refused pull stages nothing")
		prices, err := iw.alpha.ListPrices(t.Context(), billing.PriceListParams{})
		require.NoError(t, err)
		require.Empty(t, prices.Items)
		require.Empty(t, iw.subscriptions(t, customerOf(iw.alice)))
	})
}
