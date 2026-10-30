//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// A CCBill subscription id is a number: a legacy bigint column kept the
// 19-digit id CCBill sends without its leading 0.
const (
	ccbillStored   = "112233445566778899"
	ccbillProvider = "0112233445566778899"
)

// ccbillWebhookWorld is alpha with a sandbox CCBill account whose webhooks
// come from loopback: CCBill signs nothing, so the source address is its
// authentication.
func ccbillWebhookWorld(t *testing.T) *authorityWorld {
	t.Helper()
	return newAuthorityWorld(t, func(w *world) {
		w.cfg.CCBillWebhookIPAllowlist = []string{"127.0.0.1/32"}
		m := w.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"ccbill": openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: openrails.EnvironmentSandbox,
			Salt: "critical-ccbill-salt"}.PSPConfig()}
		w.cfg.Merchants["alpha"] = m
	})
}

// ccbillMemberPrice is a 10.00 USD 30-day CCBill price granting ccbill:member.
func ccbillMemberPrice(t *testing.T, client *openrails.Client) billing.PriceID {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "ccbill-member", DisplayName: "CCBill member", Entitlements: []string{"ccbill:member"}})
	require.NoError(t, err)
	price, err := client.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "ccbill-monthly", PSP: "ccbill", FlexID: "critical-flex",
		Currency: "USD", UnitAmount: ptr(int64(10_000_000)), BillingIntervalHours: ptr(720)})
	require.NoError(t, err)
	return price.ID
}

// legacyCCBillMember is a host's legacy book row: a CCBill membership paid
// through paidThrough under the id ref, with its initial sale.
func legacyCCBillMember(customer billing.CustomerID, price billing.PriceID, ref, sale string, paidThrough time.Time) (billing.DeclaredSubscription, billing.DeclaredTransaction) {
	start := paidThrough.Add(-720 * time.Hour)
	return billing.DeclaredSubscription{SourceID: "legacy-" + ref, Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: ref, StartedAt: start, PaidThrough: &paidThrough,
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: "legacy-" + ref, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paidThrough}},
		billing.DeclaredTransaction{RailSubscriptionID: ref, TransactionID: sale, Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start}
}

// importLegacyCCBill lands the members as the host's legacy book.
func importLegacyCCBill(t *testing.T, client *openrails.Client, asOf time.Time) func(billing.DeclaredSubscription, billing.DeclaredTransaction) {
	t.Helper()
	return func(sub billing.DeclaredSubscription, sale billing.DeclaredTransaction) {
		t.Helper()
		book := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: sub.Customer}},
			Subscriptions: []billing.DeclaredSubscription{sub}, Transactions: []billing.DeclaredTransaction{sale}}
		res, err := client.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Equal(t, []string{sub.SourceID}, res.Imported, "%v", res.Reasons)
	}
}

// ccbillNotice posts a CCBill webhook as CCBill sends it and returns the
// status and body.
func ccbillNotice(t *testing.T, a *authorityWorld, eventType string, fields map[string]any) (int, string) {
	t.Helper()
	body := map[string]any{"clientAccnum": "999999", "clientSubacc": "0000", "timestamp": a.clock.Now().UTC().Format(time.DateTime)}
	for k, v := range fields {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/webhooks/ccbill/999999-0000?eventType="+eventType, bytes.NewReader(raw))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	response, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(out)
}

func onlySubscription(t *testing.T, client *openrails.Client, customer billing.CustomerID) billing.Subscription {
	t.Helper()
	subs, err := client.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	return subs.Items[0]
}

func reasonOf(r billing.PSPImportRecord) string {
	if r.Reason == nil {
		return ""
	}
	return *r.Reason
}

func entitledTo(t *testing.T, client *openrails.Client, customer billing.CustomerID, entitlement string) bool {
	t.Helper()
	access, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{entitlement}})
	require.NoError(t, err)
	return access.Entitlements[entitlement]
}

func TestCCBillIdentity(t *testing.T) {
	t.Run("OR-J38-S37", func(t *testing.T) {
		a := ccbillWebhookWorld(t)
		price := ccbillMemberPrice(t, a.alpha)
		carol, dave := authtest.NewUser(t, a.auth), authtest.NewUser(t, a.auth)
		alice, bob, carolID, daveID := customerOf(a.alice), customerOf(a.bob), customerOf(carol), customerOf(dave)
		// Payments are never in the future: the business clock stays put, and
		// alice's and dave's paid periods have just ended.
		now := a.clock.Now().UTC().Truncate(time.Second)
		paid, due := now.Add(5*24*time.Hour), now.Add(-time.Hour)
		land := importLegacyCCBill(t, a.alpha, now)
		land(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due))
		land(legacyCCBillMember(bob, price, "223344556677889900", "0812233445566778802", paid))
		land(legacyCCBillMember(carolID, price, "334455667788990011", "0812233445566778803", paid))
		land(legacyCCBillMember(daveID, price, "445566778899001122", "0812233445566778804", due))
		for _, c := range []billing.CustomerID{bob, carolID} {
			require.True(t, entitledTo(t, a.alpha, c, "ccbill:member"))
		}

		t.Run("another-number-matches-nothing", func(t *testing.T) {
			for _, other := range []string{"0112233445566778898", "1122334455667788990"} {
				status, body := ccbillNotice(t, a, "Cancellation", map[string]any{"subscriptionId": other, "reason": "user", "source": "webAdmin"})
				require.NotEqual(t, http.StatusOK, status, "%s names no subscription: %s", other, body)
			}
			sub := onlySubscription(t, a.alpha, alice)
			require.Equal(t, ccbillStored, *sub.RailSubscriptionID)
			require.NotEqual(t, billing.SubscriptionStatus("canceled"), sub.Status)
			require.Nil(t, sub.CanceledAt)
		})

		t.Run("cancellation-applies-and-takes-the-providers-spelling", func(t *testing.T) {
			status, body := ccbillNotice(t, a, "Cancellation", map[string]any{"subscriptionId": "0223344556677889900", "reason": "user", "source": "webAdmin"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, bob)
			require.Equal(t, "0223344556677889900", *sub.RailSubscriptionID, "the provider's spelling wins")
			require.NotNil(t, sub.CanceledAt, "status %s", sub.Status)
			require.True(t, entitledTo(t, a.alpha, bob, "ccbill:member"), "a cancellation keeps the paid period")
		})

		t.Run("chargeback-applies-and-takes-the-providers-spelling", func(t *testing.T) {
			status, body := ccbillNotice(t, a, "Chargeback", map[string]any{"subscriptionId": "0334455667788990011", "transactionId": "0812233445566778803",
				"amount": "10.00", "currencyCode": "840", "reason": "fraud"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, carolID)
			require.Equal(t, "0334455667788990011", *sub.RailSubscriptionID)
			require.Equal(t, billing.SubscriptionStatus("canceled"), sub.Status)
			require.Equal(t, "chargeback", *sub.CancelType)
			require.False(t, entitledTo(t, a.alpha, carolID, "ccbill:member"), "a chargeback ends access now")
			payments := ccbillPayments(t, a.world, carolID)
			require.Contains(t, payments, "chargeback:0812233445566778803", "%v", payments)
		})

		t.Run("renewal-extends-paid-through-and-access", func(t *testing.T) {
			at := a.clock.Now().UTC()
			require.True(t, onlySubscription(t, a.alpha, alice).CurrentPeriodEndsAt.Equal(due))
			next := due.Add(720 * time.Hour)
			status, body := ccbillNotice(t, a, "RenewalSuccess", map[string]any{"subscriptionId": ccbillProvider, "transactionId": "0912233445566778801",
				"billedAmount": "10.00", "billedCurrencyCode": "840", "timestamp": at.Format(time.DateTime), "nextRenewalDate": next.Format(time.DateOnly)})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, alice)
			require.Equal(t, ccbillProvider, *sub.RailSubscriptionID, "the provider's spelling wins")
			require.Equal(t, billing.SubscriptionStatus("active"), sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(time.Date(next.Year(), next.Month(), next.Day(), 23, 59, 59, 0, time.UTC)), "paid through %s", sub.CurrentPeriodEndsAt)
			require.True(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
			require.EqualValues(t, 10_000_000, ccbillPayments(t, a.world, alice)["0912233445566778801"])
		})

		t.Run("a-replayed-legacy-book-names-the-respelled-row", func(t *testing.T) {
			renewed := onlySubscription(t, a.alpha, alice)
			sub, sale := legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", due)
			one := 1
			res, err := a.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "ccbill"}, SubscriptionsExhaustive: true, ExpectedSubscriptions: &one,
				Customers: []billing.DeclaredCustomer{{Customer: alice}}, Subscriptions: []billing.DeclaredSubscription{sub}, Transactions: []billing.DeclaredTransaction{sale}})
			require.NoError(t, err)
			require.Empty(t, res.Blocked, "%v", res.Reasons)
			after := onlySubscription(t, a.alpha, alice)
			require.Equal(t, ccbillProvider, *after.RailSubscriptionID, "a host book never respells a row")
			require.Equal(t, billing.SubscriptionStatus("active"), after.Status, "the book names the row it holds: nothing is absent")
			require.True(t, after.CurrentPeriodEndsAt.Equal(*renewed.CurrentPeriodEndsAt))
			require.True(t, entitledTo(t, a.alpha, alice, "ccbill:member"))
		})

		t.Run("expiration-ends-a-lapsed-membership", func(t *testing.T) {
			status, body := ccbillNotice(t, a, "Expiration", map[string]any{"subscriptionId": "0445566778899001122"})
			require.Equal(t, http.StatusOK, status, body)
			sub := onlySubscription(t, a.alpha, daveID)
			require.Equal(t, "0445566778899001122", *sub.RailSubscriptionID)
			require.Equal(t, billing.SubscriptionStatus("canceled"), sub.Status)
			require.False(t, entitledTo(t, a.alpha, daveID, "ccbill:member"))
		})
	})

	t.Run("OR-J38-S38/datalink-import-matches-a-zero-dropped-legacy-id", func(t *testing.T) {
		iw := reconcileWorld(t, "ccbill")
		price := ccbillMemberPrice(t, iw.alphaRemote)
		alice, bob := customerOf(iw.alice), customerOf(iw.bob)
		now := iw.clock.Now().UTC()
		paid := now.Truncate(24 * time.Hour).Add(5 * 24 * time.Hour)
		importLegacyCCBill(t, iw.alpha, now)(legacyCCBillMember(alice, price, ccbillStored, "0812233445566778801", paid))
		_, err := iw.alpha.SetPSPImportMappings(t.Context(), billing.SetPSPImportMappingsParams{Mappings: []billing.PSPImportMapping{
			{Mapping: "legacy_ccbill_subscription", Key: "223344556677889900", Customer: &bob}}})
		require.NoError(t, err)

		// CCBill states its next rebill as a date: the cadence it implies is 30 days.
		signup, rebilled := paid.Add(-720*time.Hour), now.Add(-10*24*time.Hour).Truncate(24*time.Hour).Add(18*time.Hour)
		next := rebilled.Add(30 * 24 * time.Hour)
		iw.ccbill.AddMember(ccbillmock.Member{SubscriptionID: ccbillProvider, Username: iw.alice.Username, Email: iw.alice.Email, Signup: signup, Rebill: &paid})
		iw.ccbill.AddMember(ccbillmock.Member{SubscriptionID: "0223344556677889900", Username: "legacy-bob", Email: "legacy-bob@critical.invalid", Signup: signup, Rebill: &next})
		iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: "0223344556677889900", At: rebilled, Amount: "10.00"})
		iw.ccbill.AddMember(ccbillmock.Member{SubscriptionID: "0112233445566778898", Username: "stranger", Email: "stranger@critical.invalid", Signup: signup, Rebill: &paid})
		since := rebilled.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since, Rules: []billing.PSPImportRule{
			{Fact: "subscription_ref", Via: billing.PSPImportViaMapping, Mapping: "legacy_ccbill_subscription"},
			{Fact: "email", Via: billing.PSPImportViaDirectory}}})

		recs := iw.records(t, imp.ID)
		require.Equal(t, billing.PSPImportHeld, recs[ccbillProvider].State, reasonOf(recs[ccbillProvider]))
		require.Equal(t, alice, *recs[ccbillProvider].Customer)
		require.Equal(t, ccbillProvider, *onlySubscription(t, iw.alpha, alice).RailSubscriptionID, "the provider's spelling wins")
		require.Equal(t, billing.PSPImportLinked, recs["0223344556677889900"].State, reasonOf(recs["0223344556677889900"]))
		require.Equal(t, "rule:1", *recs["0223344556677889900"].LinkedVia, "the legacy id maps it")
		require.Equal(t, bob, *recs["0223344556677889900"].Customer)
		require.Equal(t, billing.PSPImportUnmatched, recs["0112233445566778898"].State, "another number is another subscription")

		recs = iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs["0223344556677889900"].State, reasonOf(recs["0223344556677889900"]))
		require.Equal(t, "0223344556677889900", *onlySubscription(t, iw.alpha, bob).RailSubscriptionID)
		held := onlySubscription(t, iw.alpha, alice)
		require.Equal(t, ccbillProvider, *held.RailSubscriptionID)
		require.Equal(t, "provider", iw.held(t, ccbillProvider).Authority, "Apply hands the held host row to the provider")
		require.True(t, iw.entitled(t, alice, "ccbill:member"))
	})

	t.Run("OR-J38-S39/provider-refresh-respells-and-records-the-rebill", func(t *testing.T) {
		var feed *httptest.Server
		var rebill time.Time
		w := newWorld(t, func(w *world) {
			rebill = w.clock.Now().Truncate(time.Second).Add(-24 * time.Hour)
			feed = dataLinkFeedFor(t, ccbillProvider, "0912233445566778801", rebill)
			declareCCBill(w, feed)
		})
		price := ccbillMemberPrice(t, w.alpha)
		customer := billing.CustomerID(uuid.New())
		start := rebill.Add(-720 * time.Hour)
		book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member", Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: ccbillStored,
				StartedAt: start, PaidThrough: &rebill,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ccbillStored, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: rebill}}},
			Transactions: []billing.DeclaredTransaction{
				{RailSubscriptionID: ccbillStored, TransactionID: "0812233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
				{RailSubscriptionID: ccbillStored, TransactionID: "0912233445566778801", Type: "sale", Success: true, Currency: "USD", OccurredAt: rebill},
			}}
		res, err := w.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, res.Blocked, "%+v", res.Reasons)
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		require.Eventually(t, func() bool {
			return len(attributionJobs(t, fleet, rivertype.JobStateScheduled)) == 1
		}, 20*time.Second, 20*time.Millisecond, "the import's read waits for the pull")
		refreshMerchant(t, fleet, w.alpha.MerchantID().UUID())
		require.Equal(t, ccbillProvider, *onlySubscription(t, w.alpha, customer).RailSubscriptionID, "the roster's spelling wins")
		payments := ccbillPayments(t, w, customer)
		require.EqualValues(t, 10_000_000, payments["0912233445566778801"], "the pull records the rebill of the row it names: %v", payments)
	})

	t.Run("OR-J38-S40", func(t *testing.T) {
		var feed *httptest.Server
		var rebill time.Time
		w := newWorld(t, func(w *world) {
			rebill = w.clock.Now().Truncate(time.Second).Add(-10 * 24 * time.Hour)
			feed = dataLinkFeedFor(t, ccbillProvider, "0912233445566778801", rebill)
			declareCCBill(w, feed)
		})
		price := ccbillMemberPrice(t, w.alpha)
		customer := billing.CustomerID(uuid.New())
		start, paid := rebill.Add(-720*time.Hour), rebill.Add(720*time.Hour)
		res, err := w.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"},
			Customers: []billing.DeclaredCustomer{{Customer: customer}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member", Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: ccbillStored,
				StartedAt: start, PaidThrough: &paid,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: ccbillStored, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paid}}},
			Transactions: []billing.DeclaredTransaction{
				{RailSubscriptionID: ccbillStored, TransactionID: "0812233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
				{RailSubscriptionID: ccbillStored, TransactionID: "0912233445566778801", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: rebill},
			}})
		require.NoError(t, err)
		require.Empty(t, res.Blocked, "%+v", res.Reasons)
		require.Equal(t, billing.SubscriptionStatus("active"), onlySubscription(t, w.alpha, customer).Status)

		for _, pass := range []struct {
			name      string
			mode      reconcile.Mode
			mutations *reconcile.LocalMutationPolicy
		}{
			{"advisory-matches-by-identity", reconcile.ModeAdvisory, nil},
			{"enforce-without-insert-matches-by-identity", reconcile.ModeEnforce, &reconcile.LocalMutationPolicy{Overwrite: true}},
		} {
			t.Run(pass.name, func(t *testing.T) {
				run := ccbillPull(t, w, feed, rebill.Add(-24*time.Hour), pass.mode, pass.mutations)
				for _, f := range run.Findings {
					require.NotContains(t, []reconcile.FindingType{reconcile.FindingRemoteSubMissingLocal, reconcile.FindingLocalActiveRemoteDead, reconcile.FindingStatusMismatch},
						f.Type, "the roster names the held row: %s %s %s", f.Type, f.SubjectKey, f.RecommendedAction)
				}
				sub := onlySubscription(t, w.alpha, customer)
				require.Equal(t, billing.SubscriptionStatus("active"), sub.Status)
				require.Nil(t, sub.CanceledAt)
				require.Equal(t, ccbillStored, *sub.RailSubscriptionID, "only a mirror write respells the row")
			})
		}
	})
}

// completeRoster claims the roster it reads is complete. CCBill's never does:
// it is the strongest absence evidence a pull can hold.
type completeRoster struct{ reconcile.RailFetcher }

func (c completeRoster) Fetch(ctx context.Context, params reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	snap, err := c.RailFetcher.Fetch(ctx, params)
	if snap != nil {
		snap.Coverage.SubscriptionsExhaustive = true
	}
	return snap, err
}

// ccbillPull runs alpha's CCBill provider pull over feed in mode, with the
// policy a caller may hold that the provider refresh never does.
func ccbillPull(t *testing.T, w *world, feed *httptest.Server, since time.Time, mode reconcile.Mode, mutations *reconcile.LocalMutationPolicy) *reconcile.RunResult {
	t.Helper()
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	dl := ccbill.NewDataLinkClient(&config.CCBillConfig{ClientAccNum: "945280", ClientSubAcc: "0001", Sandbox: true,
		DataLinkUsername: "critical-datalink", DataLinkPassword: "critical-datalink-password"})
	dl.BaseURL = feed.URL
	engine := reconcile.NewEngine(database, &w.cfg, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderCCBill: completeRoster{reconcile.NewCCBillFetcher(dl)}}, nil)
	engine.Now = w.clock.Now
	psps, err := w.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	binding := reconcile.PSPBinding{ID: psps.Items[0].ID.UUID(), Rail: "ccbill", AccountID: "945280-0001"}
	var res *reconcile.RunResult
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), w.alpha.MerchantID()), func(ctx context.Context) error {
		res, err = engine.Run(ctx, reconcile.RunParams{Mode: mode, Mutations: mutations, Providers: []reconcile.Provider{reconcile.ProviderCCBill},
			PSPs: map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderCCBill: binding}, Since: since, Until: w.clock.Now()})
		return err
	}))
	return res
}

// dataLinkFeedFor is CCBill's DataLink holding one active membership sub,
// rebilled by txn at rebill.
func dataLinkFeedFor(t *testing.T, sub, txn string, rebill time.Time) *httptest.Server {
	t.Helper()
	mst := time.FixedZone("MST", -7*60*60)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.URL.Path != "/data/main.cgi" {
			http.Error(out, "unexpected DataLink request", http.StatusBadRequest)
			return
		}
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			fmt.Fprintf(out, "\"ACTIVEMEMBERS\",\"945280\",\"critical-flex\",\"%s\",\"%s\",\"member\",\"member@critical.invalid\",\"1\",\"%s\",\"\"\n",
				sub, rebill.Add(-720*time.Hour).Format("2006-01-02"), rebill.Add(720*time.Hour).Format("2006-01-02"))
			return
		}
		from, errFrom := time.ParseInLocation("20060102150405", r.Form.Get("startTime"), mst)
		until, errUntil := time.ParseInLocation("20060102150405", r.Form.Get("endTime"), mst)
		if errFrom != nil || errUntil != nil {
			http.Error(out, "unbounded DataLink export", http.StatusBadRequest)
			return
		}
		if !rebill.Before(from) && rebill.Before(until) {
			fmt.Fprintf(out, "REBILL,945280,0001,%s,%s,%s,10.00\n", sub, rebill.UTC().Format("2006-01-02 15:04:05"), txn)
		}
	}))
	t.Cleanup(server.Close)
	return server
}
