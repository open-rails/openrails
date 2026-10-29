//go:build integration

package critical_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// table is one of the world's tables, schema-qualified.
func (iw *importWorld) table(name string) string { return pgx.Identifier{iw.schema, name}.Sanitize() }

// heldRow is what a subscription records beyond the API: whose facts it
// keeps, and the schedule it accepted last.
type heldRow struct {
	Authority, Status, Schedule, Price string
	Started                            time.Time
	PeriodEnd                          *time.Time
}

func (iw *importWorld) held(t *testing.T, ref string) heldRow {
	t.Helper()
	var r heldRow
	require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT s.facts_authority, s.status, COALESCE((SELECT ps.source_reference FROM %s ps
		WHERE ps.merchant_id = s.merchant_id AND ps.subscription_id = s.id ORDER BY ps.accepted_version DESC LIMIT 1), ''),
		(SELECT p.public_id FROM %s p WHERE p.merchant_id = s.merchant_id AND p.id = s.price_id), s.started_at, s.current_period_ends_at
		FROM %s s WHERE s.merchant_id = $1 AND s.rail_subscription_id = $2 AND s.deleted_at IS NULL`, iw.table("provider_schedules"), iw.table("prices"), iw.table("subscriptions")),
		iw.alpha.MerchantID().UUID(), ref).Scan(&r.Authority, &r.Status, &r.Schedule, &r.Price, &r.Started, &r.PeriodEnd))
	return r
}

// book is what an import could land: subscriptions with their state and row
// version, payments, cards, access windows and accepted schedules, byte for
// byte.
func (iw *importWorld) book(t *testing.T) string {
	t.Helper()
	var out string
	agg := func(table, order string) string {
		return fmt.Sprintf("(SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY %s), '[]') FROM %s x WHERE x.merchant_id = $1)", order, iw.table(table))
	}
	subs := fmt.Sprintf(`(SELECT COALESCE(jsonb_agg(jsonb_build_object('id', x.id, 'customer_id', x.customer_id, 'price_id', x.price_id, 'product_id', x.product_id,
		'rail_subscription_id', x.rail_subscription_id, 'payment_method_id', x.payment_method_id, 'started_at', x.started_at, 'facts_authority', x.facts_authority,
		'status', x.status, 'current_period_starts_at', x.current_period_starts_at, 'current_period_ends_at', x.current_period_ends_at, 'ended_at', x.ended_at,
		'canceled_at', x.canceled_at, 'cancel_type', x.cancel_type, 'row_version', x.row_version) ORDER BY x.id), '[]')
		FROM %s x WHERE x.merchant_id = $1)`, iw.table("subscriptions"))
	access := fmt.Sprintf(`(SELECT COALESCE(jsonb_agg(jsonb_build_object('id', x.id, 'product_id', x.product_id, 'source_type', x.source_type, 'payment_id', x.payment_id,
		'starts_at', x.starts_at, 'ends_at', x.ends_at, 'revoked_at', x.revoked_at, 'deleted_at', x.deleted_at) ORDER BY x.id), '[]')
		FROM %s x WHERE x.merchant_id = $1)`, iw.table("product_access"))
	require.NoError(t, iw.pool.QueryRow(t.Context(), "SELECT jsonb_build_object('subscriptions', "+subs+
		", 'payments', "+agg("payments", "x.id")+", 'payment_methods', "+agg("payment_methods", "x.id")+
		", 'product_access', "+access+", 'schedules', "+agg("provider_schedules", "x.subscription_id, x.accepted_version")+")::text",
		iw.alpha.MerchantID().UUID()).Scan(&out))
	return out
}

// proposals are the open proposals one import's pull raised.
func (iw *importWorld) proposals(t *testing.T, pull billing.PSPImportID) []billing.Finding {
	t.Helper()
	page, err := iw.alpha.ListProposals(t.Context(), billing.ProposalListParams{Pull: pull, PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	require.Empty(t, page.Next)
	return page.Items
}

// kinds counts proposals by change kind, per subscription when named.
func kinds(findings []billing.Finding) map[billing.ChangeKind]int {
	out := map[billing.ChangeKind]int{}
	for _, f := range findings {
		if f.Change != nil {
			out[f.Change.Kind]++
		}
	}
	return out
}

func proposalOf(t *testing.T, findings []billing.Finding, kind billing.ChangeKind, subject string) billing.Finding {
	t.Helper()
	for _, f := range findings {
		if f.Change != nil && f.Change.Kind == kind && (subject == "" || f.Change.Subject.ID == subject) {
			return f
		}
	}
	t.Fatalf("no %s proposal for %q in %d proposals", kind, subject, len(findings))
	return billing.Finding{}
}

func subscriptionOf(t *testing.T, iw *importWorld, customer billing.CustomerID, ref string) billing.Subscription {
	t.Helper()
	for _, s := range iw.subscriptions(t, customer) {
		if s.RailSubscriptionID != nil && *s.RailSubscriptionID == ref {
			return s
		}
	}
	t.Fatalf("customer %s holds no subscription %s", customer, ref)
	return billing.Subscription{}
}

// manual pulls the PSP again, as a person reconciling it.
func (iw *importWorld) manual(t *testing.T, psp string) *billing.PSPImport {
	t.Helper()
	return iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: psp}})
}

// nmiNotice posts a signed NMI subscription event for ref.
func (iw *importWorld) nmiNotice(t *testing.T, id, ref string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"event_id": id, "event_type": "recurring.subscription.update",
		"event_body": map[string]any{"subscription_id": ref, "merchant": map[string]string{"id": "critical-import-gateway"}}})
	require.NoError(t, err)
	at := fmt.Sprint(time.Now().Unix())
	h := hmac.New(sha256.New, []byte("critical-import-webhook"))
	_, _ = h.Write([]byte(at + "."))
	_, _ = h.Write(body)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, iw.server.URL+"/v1/webhooks/nmi/critical-import-gateway", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Webhook-Signature", "t="+at+",s="+hex.EncodeToString(h.Sum(nil)))
	response, err := iw.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
}

// premium is one product with an NMI provider price per plan.
func (iw *importWorld) premium(t *testing.T, plans ...string) map[string]*billing.Price {
	t.Helper()
	product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "premium", DisplayName: "Premium", Entitlements: []string{"member:premium"}})
	require.NoError(t, err)
	out := map[string]*billing.Price{}
	for _, plan := range plans {
		out[plan], err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-" + plan, PSP: "nmi", PlanID: plan, Archived: true})
		require.NoError(t, err)
	}
	return out
}

// hostBook lands a host's legacy book of NMI memberships.
func (iw *importWorld) hostBook(t *testing.T, asOf time.Time, subs ...billing.DeclaredSubscription) billing.BillingImportResult {
	t.Helper()
	book := billing.DeclaredBilling{AsOf: asOf, DefaultPSP: billing.PSPRef{Key: "nmi"}, Subscriptions: subs}
	for _, s := range subs {
		book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: s.Customer})
	}
	res, err := iw.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	return *res
}

func TestPSPReconcile(t *testing.T) {
	t.Run("OR-J38-S20/manual-reconcile-proposes-and-writes-nothing", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := openPeriod(iw)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "silver", Name: "Silver", Amount: "10.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "retired", Name: "Retired", Amount: "15.00", Days: 30})
		carol := authtest.NewUser(t, iw.auth)
		renewed := iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		gone := iw.nmiMember(t, iw.bob.Email, "gold", "20.00", start)
		recarded := iw.nmiMember(t, carol.Email, "gold", "20.00", start)
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		for _, key := range []string{"gold", "silver"} {
			product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
			_, err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-" + key, PSP: "nmi", PlanID: key, Archived: true})
			require.NoError(t, err)
		}
		require.NoError(t, iw.link(t, first.ID, billing.LinkPSPImportParams{}))
		recs := iw.apply(t, first.ID)
		for _, ref := range []string{renewed, gone, recarded} {
			require.Equal(t, billing.PSPImportImported, recs[ref].State, "%v", recs[ref].Reason)
			require.Equal(t, "provider", iw.held(t, ref).Authority, "a PSP import lands provider facts")
		}

		// The provider moves on: a renewal OpenRails missed, a deleted
		// schedule, a replaced card, a new schedule on a vault OpenRails
		// holds, and a plan gone from the plan list.
		// The provider billed the renewal OpenRails has not seen.
		iw.renewEarly(renewed)
		iw.renewEarly(recarded)
		iw.nmi.DeleteSchedule(gone)
		vault := iw.nmi.Schedule(recarded).Vault
		iw.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card = nmimock.Card{Brand: "visa", Last4: "1111", Exp: "0131"} })
		added := iw.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "silver", Amount: "10.00", Days: 30, Created: iw.clock.Now(), NextBilling: iw.clock.Now().AddDate(0, 0, 30), Order: "order-silver-" + vault})
		iw.nmi.AddScheduleSale(added, iw.clock.Now())
		iw.nmi.RemovePlan("retired")

		writes := len(iw.nmi.Calls())
		before := iw.book(t)
		manual := iw.manual(t, "nmi")
		if diff := bookDiff(t, before, iw.book(t)); len(diff) > 0 {
			jobs, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().First(200))
			require.NoError(t, err)
			var ran []string
			for _, j := range jobs.Jobs {
				ran = append(ran, j.Kind+":"+string(j.State))
			}
			t.Fatalf("a pull lands nothing, but %v; jobs %v", diff, ran)
		}
		recs = iw.records(t, manual.ID)
		require.Equal(t, billing.PSPImportLinked, recs[added].State, "%v", recs[added].Reason)
		for _, ref := range []string{renewed, recarded, vault} {
			require.Equal(t, billing.PSPImportHeld, recs[ref].State, "%s: %v", ref, recs[ref].Reason)
			require.Equal(t, "held", *recs[ref].LinkedVia)
		}
		require.NotContains(t, recs, gone, "a deleted schedule leaves the roster")
		retired := iw.catalog(t, billing.ProviderCatalogListParams{PSP: billing.PSPRef{ID: &manual.PSP}})["price:retired"]
		require.NotNil(t, retired.DeletedAt, "a complete list lacking a plan marks it deleted")
		require.True(t, retired.Retired)

		open := iw.proposals(t, manual.ID)
		got := kinds(open)
		require.Equal(t, 2, got[billing.ChangeRecordCharge], "%v", got)
		require.Equal(t, 2, got[billing.ChangeAdoptSubscription], "%v", got)
		require.Equal(t, 1, got[billing.ChangeCancelSubscription], "%v %s", got, iw.findingDump(t))
		require.Equal(t, 1, got[billing.ChangeUpdateCard], "%v", got)
		for _, f := range open {
			require.Equal(t, manual.ID, *f.Pull, "every proposal names its pull")
			require.Equal(t, billing.ProposalSourcePull, *f.Source)
		}
		cancel := proposalOf(t, open, billing.ChangeCancelSubscription, subscriptionOf(t, iw, customerOf(iw.bob), gone).ID.String())
		require.True(t, cancel.Change.Effects.EndsAccess)
		view, err := iw.alpha.GetPSPImport(t.Context(), manual.ID)
		require.NoError(t, err)
		require.Equal(t, billing.PSPImportProposals{Open: 6, Corrections: 5}, view.Proposals)

		recs = iw.apply(t, manual.ID)
		require.Equal(t, billing.PSPImportImported, recs[added].State, "%v", recs[added].Reason)
		require.Len(t, iw.subscriptions(t, customerOf(carol)), 2, "the new schedule landed")
		var run *billing.RunID
		outcomes := map[billing.ChangeKind][]string{}
		for _, f := range open {
			if f.ID == cancel.ID {
				continue
			}
			now, err := iw.alpha.GetFinding(t.Context(), f.ID)
			require.NoError(t, err)
			require.NotNil(t, now.Resolution, "%s %s is %s: %s", f.Change.Kind, f.Change.Subject.ID, now.Status, deref(now.Notes))
			outcomes[f.Change.Kind] = append(outcomes[f.Change.Kind], *now.Resolution+" "+fmt.Sprint(now.Evidence["resolution"]))
			if *now.Resolution != "approved" {
				continue
			}
			if run == nil {
				run = now.ApprovalRunID
			}
			require.Equal(t, *run, *now.ApprovalRunID, "one approval run for the import's corrections")
		}
		t.Logf("corrections: %v", outcomes)
		for _, kind := range []billing.ChangeKind{billing.ChangeRecordCharge, billing.ChangeUpdateCard} {
			for _, o := range outcomes[kind] {
				require.True(t, strings.HasPrefix(o, "approved"), "%s: %v", kind, outcomes)
			}
		}
		left := iw.proposals(t, manual.ID)
		require.Len(t, left, 1, "ending access waits for a person")
		require.Equal(t, cancel.ID, left[0].ID)
		require.True(t, iw.entitled(t, customerOf(iw.bob), "member:gold"))
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:gold"), "the recorded renewal pays for access")
		view, err = iw.alpha.GetPSPImport(t.Context(), manual.ID)
		require.NoError(t, err)
		require.Equal(t, billing.PSPImportProposals{Open: 1, Approved: 3, Superseded: 2}, view.Proposals, "renewals wait for their settled bill")
		require.Len(t, iw.nmi.Calls(), writes, "the pull, Apply and its corrections write nothing to NMI")

		// Applying again, as a retried Apply does, lands and approves nothing more.
		again := iw.book(t)
		iw.apply(t, manual.ID)
		require.Empty(t, bookDiff(t, again, iw.book(t)), "Apply again lands nothing")
		view, err = iw.alpha.GetPSPImport(t.Context(), manual.ID)
		require.NoError(t, err)
		require.Equal(t, billing.PSPImportProposals{Open: 1, Approved: 3, Superseded: 2}, view.Proposals, "Apply again approves nothing")
	})

	t.Run("OR-J38-S21/legacy-book-then-pull-provider-wins", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "legacy23", Name: "Legacy", Amount: "23.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "current19", Name: "Current", Amount: "19.00", Days: 30})
		prices := iw.premium(t, "legacy23", "current19")
		schedule := iw.nmiMember(t, iw.alice.Email, "current19", "19.00", start)
		ended := iw.nmiMember(t, iw.bob.Email, "current19", "19.00", start)
		iw.nmi.DeleteSchedule(ended)
		// The book's paid-through is stale: NMI bills next later.
		stale, live := iw.clock.Now().Add(5*24*time.Hour), start.Add(30*24*time.Hour)
		res := iw.hostBook(t, start.Add(24*time.Hour),
			billing.DeclaredSubscription{SourceID: "legacy-alice", Customer: customerOf(iw.alice), Price: prices["legacy23"].ID, Rail: "nmi", RailSubscriptionID: schedule,
				StartedAt: start.Add(5 * 24 * time.Hour), PaidThrough: &stale},
			billing.DeclaredSubscription{SourceID: "legacy-bob", Customer: customerOf(iw.bob), Price: prices["current19"].ID, Rail: "nmi", RailSubscriptionID: ended,
				StartedAt: start, PaidThrough: &live})
		require.ElementsMatch(t, []string{"legacy-alice", "legacy-bob"}, res.Imported, "%v", res.Reasons)
		require.Equal(t, "host", iw.held(t, schedule).Authority)

		imp := iw.manual(t, "nmi")
		open := iw.proposals(t, imp.ID)
		alice := subscriptionOf(t, iw, customerOf(iw.alice), schedule)
		correct := proposalOf(t, open, billing.ChangeCorrectSubscription, alice.ID.String())
		require.False(t, correct.Change.Effects.EndsAccess, "the same product keeps access")
		require.Equal(t, prices["current19"].ID.String(), correct.Change.To["price_id"])
		require.NotNil(t, correct.Change.To["schedule"])
		require.NotNil(t, correct.Change.To["started_at"])
		bob := subscriptionOf(t, iw, customerOf(iw.bob), ended)
		cancel := proposalOf(t, open, billing.ChangeCancelSubscription, bob.ID.String())

		iw.apply(t, imp.ID)
		applied, err := iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.Equal(t, "approved", *applied.Resolution, "notes %v, evidence %v", applied.Notes, applied.Evidence["resolution"])
		row := iw.held(t, schedule)
		require.Equal(t, prices["current19"].ID.String(), row.Price, "the provider's mapped price")
		require.Equal(t, "nmi_subscription:"+schedule, row.Schedule, "the provider's schedule terms")
		require.True(t, row.Started.Equal(start), "the provider's start: %s", row.Started)
		require.Equal(t, "provider", row.Authority)
		require.WithinDuration(t, stale, *row.PeriodEnd, time.Second, "the period moves only on a renewal charge; findings %s", iw.findingDump(t))
		require.Equal(t, "active", row.Status)
		require.Equal(t, "active", iw.held(t, ended).Status, "an ended schedule keeps access until a person approves")
		require.True(t, iw.entitled(t, customerOf(iw.bob), "member:premium"))

		out, err := iw.alpha.ApproveProposals(t.Context(), billing.ApproveProposalsParams{IDs: []billing.FindingID{cancel.ID}, Notes: "ended at NMI"}, openrails.WithIdempotencyKey("end-bob-held"))
		require.NoError(t, err)
		require.Equal(t, billing.ProposalHeld, out.Data[0].Outcome, "the kill switch holds an end of access")
		enableLifecycleMaintenance(t, iw.authorityWorld)
		out, err = iw.alpha.ApproveProposals(t.Context(), billing.ApproveProposalsParams{IDs: []billing.FindingID{cancel.ID}, Notes: "ended at NMI"}, openrails.WithIdempotencyKey("end-bob"))
		require.NoError(t, err)
		require.Equal(t, billing.ProposalApproved, out.Data[0].Outcome, "%+v", out.Data[0].Error)
		require.Equal(t, "canceled", iw.held(t, ended).Status)
		require.Equal(t, "provider", iw.held(t, ended).Authority, "approving a proposal hands the row to the provider")
		var grace, beyond int
		require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FILTER (WHERE source_type = 'grace'), count(*) FILTER (WHERE ends_at IS NULL OR ends_at > $3)
			FROM %s WHERE merchant_id = $1 AND customer_id = $2 AND deleted_at IS NULL AND revoked_at IS NULL`, iw.table("product_access")),
			iw.alpha.MerchantID().UUID(), customerOf(iw.bob).UUID(), live).Scan(&grace, &beyond))
		require.Zero(t, grace+beyond, "approving the cancel ends access at the paid-through date: %s", iw.accessDump(t, customerOf(iw.bob)))

		// Undoing Apply's approval run restores the corrected row's price and
		// start. The provider schedule it accepted stays: accepted schedule
		// versions are immutable.
		undo := func(args ...string) string {
			return hostCLI(t, iw.world, append([]string{"undo-run", "--merchant", "id:" + iw.alpha.MerchantID().String(), "--run", applied.ApprovalRunID.String()}, args...)...)
		}
		rows := regexp.MustCompile(`--expect-rows (\d+)`).FindStringSubmatch(undo())
		require.Len(t, rows, 2)
		undo("--apply", "--expect-rows", rows[1])
		row = iw.held(t, schedule)
		require.Equal(t, prices["legacy23"].ID.String(), row.Price, "undo restores the price")
		require.True(t, row.Started.Equal(start.Add(5*24*time.Hour)), "undo restores the start: %s", row.Started)
		require.Equal(t, "nmi_subscription:"+schedule, row.Schedule, "the accepted provider schedule stays")
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:premium"))
	})

	t.Run("OR-J38-S22/pull-then-legacy-book-is-skipped", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "current19", Name: "Current", Amount: "19.00", Days: 30})
		prices := iw.premium(t, "current19")
		schedule := iw.nmiMember(t, iw.alice.Email, "current19", "19.00", start)
		since := start.Add(-24 * time.Hour)
		imp := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		iw.apply(t, imp.ID)
		require.Equal(t, "provider", iw.held(t, schedule).Authority)
		before := iw.book(t)

		older := start.Add(3 * 24 * time.Hour)
		carol := authtest.NewUser(t, iw.auth)
		res := iw.hostBook(t, start.Add(24*time.Hour),
			billing.DeclaredSubscription{SourceID: "legacy-alice", Customer: customerOf(iw.alice), Price: prices["current19"].ID, Rail: "nmi", RailSubscriptionID: schedule,
				StartedAt: start, PaidThrough: &older, Cancel: billing.CancelEvidence{Kind: "user_canceled", At: older}})
		require.Equal(t, []string{"legacy-alice"}, res.Skipped)
		require.Equal(t, "provider_authority", res.Reasons["legacy-alice"])
		require.Equal(t, before, iw.book(t), "a host book never rewrites a provider row: row, schedule, charges and access")
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:premium"))

		paid := iw.clock.Now().Add(20 * 24 * time.Hour)
		res = iw.hostBook(t, iw.clock.Now(), billing.DeclaredSubscription{SourceID: "legacy-carol", Customer: customerOf(carol), Price: prices["current19"].ID, Rail: "nmi",
			RailSubscriptionID: "7000001", StartedAt: start, PaidThrough: &paid})
		require.Equal(t, []string{"legacy-carol"}, res.Imported, "%v", res.Reasons)
		require.Equal(t, "host", iw.held(t, "7000001").Authority, "a row new to OpenRails lands the host's")

		// A webhook converge reads the provider: the row is the provider's
		// from then on, and the next legacy book skips it.
		vault := iw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		iw.nmi.PutVault(nmimock.Vault{ID: vault, Card: nmimock.Card{Brand: "visa", Last4: "4242"}, Email: carol.Email})
		iw.nmi.AddSchedule(nmimock.Schedule{ID: "7000001", Vault: vault, Plan: "current19", Amount: "19.00", Days: 30, Created: start, NextBilling: paid})
		iw.nmiNotice(t, "evt_carol", "7000001")
		require.Eventually(t, func() bool { return iw.held(t, "7000001").Authority == "provider" }, 30*time.Second, 50*time.Millisecond)
		res = iw.hostBook(t, iw.clock.Now(), billing.DeclaredSubscription{SourceID: "legacy-carol", Customer: customerOf(carol), Price: prices["current19"].ID, Rail: "nmi",
			RailSubscriptionID: "7000001", StartedAt: start, PaidThrough: &older, Cancel: billing.CancelEvidence{Kind: "user_canceled", At: older}})
		require.Equal(t, "provider_authority", res.Reasons["legacy-carol"])
		require.Equal(t, "active", iw.held(t, "7000001").Status)
	})

	t.Run("OR-J38-S23/legacy-row-landed-between-pull-and-apply-is-corrected-at-apply", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "legacy23", Name: "Legacy", Amount: "23.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "current19", Name: "Current", Amount: "19.00", Days: 30})
		prices := iw.premium(t, "legacy23", "current19")
		schedule := iw.nmiMember(t, iw.alice.Email, "current19", "19.00", start)
		imp := iw.manual(t, "nmi")
		require.Equal(t, billing.PSPImportLinked, iw.records(t, imp.ID)[schedule].State)

		stale := start.Add(9 * 24 * time.Hour)
		res := iw.hostBook(t, start.Add(24*time.Hour), billing.DeclaredSubscription{SourceID: "legacy-alice", Customer: customerOf(iw.alice), Price: prices["legacy23"].ID,
			Rail: "nmi", RailSubscriptionID: schedule, StartedAt: start.Add(5 * 24 * time.Hour), PaidThrough: &stale})
		require.Equal(t, []string{"legacy-alice"}, res.Imported, "%v", res.Reasons)

		recs := iw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportHeld, recs[schedule].State, "Apply finds it held, not landed twice: %v", recs[schedule].Reason)
		require.Len(t, iw.subscriptions(t, customerOf(iw.alice)), 1)
		row := iw.held(t, schedule)
		require.Equal(t, prices["current19"].ID.String(), row.Price, "diffed and corrected at Apply")
		require.Equal(t, "nmi_subscription:"+schedule, row.Schedule)
		require.Equal(t, "provider", row.Authority)
	})

	t.Run("OR-J38-S24/reconcile-skips-rows-moved-since-the-pull", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := openPeriod(iw)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "silver", Name: "Silver", Amount: "10.00", Days: 30})
		iw.premium(t, "gold", "silver")
		schedule := iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		iw.apply(t, first.ID)
		// NMI moved the schedule to another mapped plan and billed it.
		iw.nmi.EditSchedule(schedule, func(s *nmimock.Schedule) { s.Plan, s.Amount = "silver", "10.00" })
		iw.renewEarly(schedule)
		version := func() int64 {
			var v int64
			require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT row_version FROM %s WHERE merchant_id = $1 AND rail_subscription_id = $2`, iw.table("subscriptions")),
				iw.alpha.MerchantID().UUID(), schedule).Scan(&v))
			return v
		}

		// The roster is read, then the history read fails. Meanwhile another
		// writer moves the row.
		iw.nmi.QueryUnavailable(true)
		imp, err := iw.alphaRemote.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			got, err := iw.alpha.GetPSPImport(t.Context(), imp.ID)
			return err == nil && got.PulledAt != nil
		}, 30*time.Second, 50*time.Millisecond)
		read := version()
		_, err = iw.pool.Exec(t.Context(), fmt.Sprintf(`UPDATE %s SET cancel_feedback = 'support note' WHERE merchant_id = $1 AND rail_subscription_id = $2`, iw.table("subscriptions")),
			iw.alpha.MerchantID().UUID(), schedule)
		require.NoError(t, err)
		require.Greater(t, version(), read)
		iw.nmi.QueryUnavailable(false)
		iw.await(t, imp.ID, billing.PSPImportPulled)

		open := iw.proposals(t, imp.ID)
		for _, f := range open {
			require.False(t, f.Change != nil && f.Change.Basis.RowVersion != nil, "no state change for a row moved since the roster read: %s", f.Change.Kind)
		}
		require.Zero(t, kinds(open)[billing.ChangeCorrectSubscription], "the plan move waits for the next reconcile")
		rec := iw.records(t, imp.ID)[schedule]
		require.Equal(t, billing.PSPImportHeld, rec.State)
		require.NotNil(t, rec.Reason, "row %+v; findings %s", iw.held(t, schedule), iw.findingDump(t))
		require.Equal(t, 1, kinds(open)[billing.ChangeRecordCharge], "its money facts are still proposed: %v", kinds(open))
		require.Contains(t, *rec.Reason, "changed after the pull read it")
	})

	t.Run("OR-J38-S25/product-change-waits-for-a-person", func(t *testing.T) {
		iw := reconcileWorld(t, "stripe")
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddProduct("prod_pro", "Pro")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		iw.stripe.AddPrice(stripemock.Price{ID: "price_pro", Product: "prod_pro", Amount: 1999, Interval: "month"})
		prices := map[string]*billing.Price{}
		for rank, key := range []string{"basic", "pro"} {
			product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: new("membership"), TierRank: rank + 1, Entitlements: []string{"member:" + key}})
			require.NoError(t, err)
			prices[key], err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-" + key, PSP: "stripe", PriceID: "price_" + key})
			require.NoError(t, err)
		}
		start := iw.clock.Now().Add(-10 * 24 * time.Hour)
		customer := iw.stripe.AddCustomer(iw.alice.Email, "Alice", nil)
		sub := iw.stripe.AddSubscription(customer, "pm_alice", "price_basic", 999, start, start.AddDate(0, 1, 0))
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since})
		iw.apply(t, first.ID)
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:basic"))

		// A move without a paid invoice for the new price proposes nothing.
		iw.stripe.PortalPriceChange(sub, "price_pro", 1999, 1000)
		writes := len(iw.stripe.Mutations(""))
		unpaid := iw.manual(t, "stripe")
		require.Zero(t, kinds(iw.proposals(t, unpaid.ID))[billing.ChangeCorrectSubscription])

		iw.stripe.PortalPriceChangePaid(sub, "price_pro", 1999, 1000)
		imp := iw.manual(t, "stripe")
		held := subscriptionOf(t, iw, customerOf(iw.alice), sub)
		correct := proposalOf(t, iw.proposals(t, imp.ID), billing.ChangeCorrectSubscription, held.ID.String())
		require.True(t, correct.Change.Effects.EndsAccess, "another product ends the old one's access")
		iw.apply(t, imp.ID)
		require.Equal(t, prices["basic"].ID, subscriptionOf(t, iw, customerOf(iw.alice), sub).PriceID, "Apply leaves a product change open")
		require.Len(t, iw.proposals(t, imp.ID), 1)

		enableLifecycleMaintenance(t, iw.authorityWorld)
		out, err := iw.alpha.ApproveProposals(t.Context(), billing.ApproveProposalsParams{IDs: []billing.FindingID{correct.ID}}, openrails.WithIdempotencyKey("move-alice"))
		require.NoError(t, err)
		require.Equal(t, billing.ProposalApproved, out.Data[0].Outcome, "%+v", out.Data[0].Error)
		require.Equal(t, prices["pro"].ID, subscriptionOf(t, iw, customerOf(iw.alice), sub).PriceID)
		var grace int
		require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s a JOIN %s p ON p.merchant_id = a.merchant_id AND p.id = a.product_id
			WHERE a.merchant_id = $1 AND a.customer_id = $2 AND a.source_type = 'grace' AND a.revoked_at IS NULL AND a.deleted_at IS NULL AND p.key = 'basic'`,
			iw.table("product_access"), iw.table("products")), iw.alpha.MerchantID().UUID(), customerOf(iw.alice).UUID()).Scan(&grace))
		require.Zero(t, grace, "the old product's grace is revoked: %s", iw.accessDump(t, customerOf(iw.alice)))
		require.Len(t, iw.stripe.Mutations(""), writes, "the pulls, Apply and the approval write nothing to Stripe")
	})

	t.Run("OR-J38-S26/reconcile-refuses-an-implausible-roster", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.premium(t, "gold")
		var schedules []string
		for range 12 {
			u := authtest.NewUser(t, iw.auth)
			schedules = append(schedules, iw.nmiMember(t, u.Email, "gold", "20.00", start))
		}
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		iw.apply(t, first.ID)
		// Wrong credentials read another account: few schedules, another
		// plan list.
		for _, s := range schedules[1:] {
			iw.nmi.DeleteSchedule(s)
		}
		iw.nmi.AddPlan(nmimock.Plan{ID: "foreign", Name: "Foreign", Amount: "5.00", Days: 30})
		iw.nmi.RemovePlan("gold")
		iw.clock.Advance(time.Minute)
		imp, err := iw.alphaRemote.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.NoError(t, err)
		got := iw.awaitFailed(t, imp.ID)
		require.Equal(t, billing.PSPImportRosterImplausible, *got.ErrorCode, "%v", got.Error)
		require.Empty(t, iw.proposals(t, imp.ID), "an implausible roster raises nothing")
		objects := iw.catalog(t, billing.ProviderCatalogListParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.Nil(t, objects["price:gold"].DeletedAt, "an implausible roster marks no plan gone")
		require.NotContains(t, objects, "price:foreign", "nor records the other account's")
		page, err := iw.alpha.ListProposals(t.Context(), billing.ProposalListParams{PageRequest: billing.PageRequest{Limit: 100}})
		require.NoError(t, err)
		require.Empty(t, page.Items)
	})

	t.Run("OR-J38-S27/proposal-routes-admit-a-person", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := openPeriod(iw)
		iw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		iw.premium(t, "gold")
		renewed := iw.nmiMember(t, iw.alice.Email, "gold", "20.00", start)
		gone := iw.nmiMember(t, iw.bob.Email, "gold", "20.00", start)
		since := start.Add(-24 * time.Hour)
		first := iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since})
		iw.apply(t, first.ID)
		iw.renewEarly(renewed)
		iw.nmi.DeleteSchedule(gone)
		imp := iw.manual(t, "nmi")

		// A later pull supersedes the first import: it is linked and applied
		// no more, and the later import keeps its proposals.
		raised := iw.proposals(t, imp.ID)
		_, err := iw.api.LinkPSPImport(t.Context(), first.ID, billing.LinkPSPImportParams{})
		require.ErrorIs(t, err, billing.ErrConflict, "relinking a superseded import")
		_, err = iw.alpha.ApplyPSPImport(t.Context(), first.ID)
		require.ErrorIs(t, err, billing.ErrConflict, "applying a superseded import")
		require.Equal(t, raised, iw.proposals(t, imp.ID))

		key := iw.alphaKey.Secret
		status, raw := iw.request(t, http.MethodGet, "/v1/merchant/proposals?pull_id="+imp.ID.String(), key, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var page billing.ListPage[billing.Finding]
		require.NoError(t, json.Unmarshal(raw, &page))
		require.NotEmpty(t, page.Items, "an API key lists proposals")
		charge := proposalOf(t, page.Items, billing.ChangeRecordCharge, "")
		cancel := proposalOf(t, page.Items, billing.ChangeCancelSubscription, "")

		approve := map[string]any{"ids": []string{charge.ID.String()}}
		for _, path := range []string{"/v1/merchant/proposals/approve", "/v1/merchant/proposals/dismiss", "/v1/merchant/psp-imports/" + imp.ID.String() + "/apply"} {
			status, raw = iw.request(t, http.MethodPost, path, key, map[string]any{"ids": []string{charge.ID.String()}, "notes": "automation"}, map[string]string{"Idempotency-Key": "key-" + path})
			require.Equal(t, http.StatusForbidden, status, "%s: %s", path, raw)
			require.Contains(t, string(raw), billing.CodePersonRequired)
		}
		status, raw = iw.request(t, http.MethodPost, "/v1/merchant/proposals/approve", iw.aliceToken, approve)
		require.Equal(t, http.StatusBadRequest, status, "approve needs an Idempotency-Key: %s", raw)
		var ids []string
		for range billing.MaxBatchItems + 1 {
			ids = append(ids, charge.ID.String())
		}
		status, _ = iw.request(t, http.MethodPost, "/v1/merchant/proposals/approve", iw.aliceToken, map[string]any{"ids": ids}, map[string]string{"Idempotency-Key": "too-many"})
		require.Equal(t, http.StatusBadRequest, status)
		status, raw = iw.request(t, http.MethodPost, "/v1/merchant/proposals/approve", iw.aliceToken, approve, map[string]string{"Idempotency-Key": "approve-charge"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var approved billing.ProposalResults
		require.NoError(t, json.Unmarshal(raw, &approved))
		require.Equal(t, billing.ProposalApproved, approved.Data[0].Outcome, "%+v", approved.Data[0].Error)
		status, again := iw.request(t, http.MethodPost, "/v1/merchant/proposals/approve", iw.aliceToken, approve, map[string]string{"Idempotency-Key": "approve-charge"})
		require.Equal(t, http.StatusOK, status)
		require.JSONEq(t, string(raw), string(again), "a replay answers the first response")
		status, raw = iw.request(t, http.MethodPost, "/v1/merchant/proposals/approve", iw.aliceToken, map[string]any{"ids": []string{cancel.ID.String()}}, map[string]string{"Idempotency-Key": "approve-charge"})
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
		require.Contains(t, string(raw), billing.CodeIdempotencyKeyReused)

		status, raw = iw.request(t, http.MethodPost, "/v1/merchant/proposals/dismiss", iw.aliceToken, map[string]any{"ids": []string{cancel.ID.String()}})
		require.Equal(t, http.StatusBadRequest, status, "dismissing needs notes: %s", raw)
		status, raw = iw.request(t, http.MethodPost, "/v1/merchant/proposals/dismiss", iw.aliceToken, map[string]any{"ids": []string{cancel.ID.String()}, "notes": "kept by support"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var dismissed billing.ProposalResults
		require.NoError(t, json.Unmarshal(raw, &dismissed))
		require.Equal(t, billing.ProposalDismissed, dismissed.Data[0].Outcome)

		iw.nmi.QueryUnavailable(true)
		busy, err := iw.alphaRemote.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.NoError(t, err)
		_, err = iw.alphaRemote.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		status, code := refusalCode(t, err)
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, billing.CodePSPImportInProgress, code)
		require.ErrorIs(t, err, billing.ErrPSPImportInProgress)
		_, err = iw.alpha.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.ErrorIs(t, err, billing.ErrPSPImportInProgress, "in-process too")
		require.Equal(t, billing.PSPImportPulling, busy.Status)
		iw.nmi.QueryUnavailable(false)
		iw.await(t, busy.ID, billing.PSPImportPulled)
		status, raw = iw.request(t, http.MethodPost, "/v1/merchant/psp-imports/"+busy.ID.String()+"/apply", iw.aliceToken, nil)
		require.Equal(t, http.StatusAccepted, status, "a person applies: %s", raw)
		iw.await(t, busy.ID, billing.PSPImportApplied)

		status, _ = iw.request(t, http.MethodPost, "/v1/merchant/psps/refresh", iw.aliceToken, nil)
		require.Equal(t, http.StatusMethodNotAllowed, status, "no refresh route: the pull is a PSP import (/psps/{id} takes no POST)")
		held := 0
		for _, r := range iw.records(t, busy.ID) {
			if r.State == billing.PSPImportHeld {
				held++
			}
		}
		require.Positive(t, held, "what OpenRails holds stays held")
	})

	t.Run("OR-J38-S28/a-dismissed-correction-is-never-applied", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := openPeriod(iw)
		for _, p := range []nmimock.Plan{{ID: "legacy23", Name: "Legacy", Amount: "23.00", Days: 30}, {ID: "current19", Name: "Current", Amount: "19.00", Days: 30},
			{ID: "current17", Name: "Promo", Amount: "17.00", Days: 30}} {
			iw.nmi.AddPlan(p)
		}
		prices := iw.premium(t, "legacy23", "current19", "current17")
		schedule := iw.nmiMember(t, iw.alice.Email, "current19", "19.00", start)
		paid := start.AddDate(0, 0, 30)
		res := iw.hostBook(t, start.Add(24*time.Hour), billing.DeclaredSubscription{SourceID: "legacy-alice", Customer: customerOf(iw.alice), Price: prices["legacy23"].ID,
			Rail: "nmi", RailSubscriptionID: schedule, StartedAt: start, PaidThrough: &paid})
		require.Equal(t, []string{"legacy-alice"}, res.Imported, "%v", res.Reasons)
		imp := iw.manual(t, "nmi")
		alice := subscriptionOf(t, iw, customerOf(iw.alice), schedule)
		correct := proposalOf(t, iw.proposals(t, imp.ID), billing.ChangeCorrectSubscription, alice.ID.String())
		out, err := iw.alpha.DismissProposals(t.Context(), billing.DismissProposalsParams{IDs: []billing.FindingID{correct.ID}, Notes: "the legacy price stands"})
		require.NoError(t, err)
		require.Equal(t, billing.ProposalDismissed, out.Data[0].Outcome)

		// NMI bills the next period: its record moves, the facts do not.
		iw.renewEarly(schedule)
		again := iw.manual(t, "nmi")
		iw.apply(t, again.ID)
		got, err := iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.Equal(t, "dismissed", deref(got.Resolution), "the same facts stay dismissed")
		require.Equal(t, prices["legacy23"].ID.String(), iw.held(t, schedule).Price)

		// NMI moves to another plan: the correction opens again, for a person.
		iw.nmi.EditSchedule(schedule, func(s *nmimock.Schedule) { s.Plan, s.Amount = "current17", "17.00" })
		third := iw.manual(t, "nmi")
		got, err = iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.Nil(t, got.Resolution, "other facts open it again")
		require.Equal(t, prices["current17"].ID.String(), got.Change.To["price_id"])
		iw.apply(t, third.ID)
		got, err = iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.Nil(t, got.Resolution, "Apply never approves a proposal a person dismissed")
		require.Equal(t, prices["legacy23"].ID.String(), iw.held(t, schedule).Price)
	})

	t.Run("OR-J38-S29/provider-refresh-leaves-a-pulls-proposals", func(t *testing.T) {
		iw := reconcileWorld(t, "nmi")
		start := iw.clock.Now().Add(-10 * 24 * time.Hour).Truncate(24 * time.Hour)
		iw.nmi.AddPlan(nmimock.Plan{ID: "legacy23", Name: "Legacy", Amount: "23.00", Days: 30})
		iw.nmi.AddPlan(nmimock.Plan{ID: "current19", Name: "Current", Amount: "19.00", Days: 30})
		prices := iw.premium(t, "legacy23", "current19")
		schedule := iw.nmiMember(t, iw.alice.Email, "current19", "19.00", start)
		stale := iw.clock.Now().Add(5 * 24 * time.Hour)
		res := iw.hostBook(t, start.Add(24*time.Hour), billing.DeclaredSubscription{SourceID: "legacy-alice", Customer: customerOf(iw.alice), Price: prices["legacy23"].ID,
			Rail: "nmi", RailSubscriptionID: schedule, StartedAt: start.Add(5 * 24 * time.Hour), PaidThrough: &stale})
		require.Equal(t, []string{"legacy-alice"}, res.Imported, "%v", res.Reasons)
		imp := iw.manual(t, "nmi")
		before := iw.pulled(t, imp.ID)
		require.Contains(t, before, string(billing.ChangeCorrectSubscription))

		// The legacy refresh reads the same PSP between the pull and Apply.
		refreshMerchant(t, iw.fleet, iw.alpha.MerchantID().UUID())
		require.Equal(t, before, iw.pulled(t, imp.ID), "the refresh leaves the pull's proposals")
		var engine int
		require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT count(*) FROM %s WHERE merchant_id = $1 AND finding_type = 'pull.subscription.facts' AND pull_id IS NULL`,
			iw.table("reconciliation_findings")), iw.alpha.MerchantID().UUID()).Scan(&engine))
		require.Zero(t, engine, "the refresh proposes no fact corrections")

		iw.apply(t, imp.ID)
		changes := func(rows string) map[string]string {
			var list []struct {
				ID     string          `json:"id"`
				Change json.RawMessage `json:"change"`
			}
			require.NoError(t, json.Unmarshal([]byte(rows), &list))
			out := map[string]string{}
			for _, r := range list {
				out[r.ID] = string(r.Change)
			}
			return out
		}
		require.Equal(t, changes(before), changes(iw.pulled(t, imp.ID)), "Apply approves the changes the pull proposed")
		correct := proposalOf(t, iw.proposalsAll(t, imp.ID), billing.ChangeCorrectSubscription, subscriptionOf(t, iw, customerOf(iw.alice), schedule).ID.String())
		require.NotNil(t, correct.Resolution)
		require.Equal(t, "approved", *correct.Resolution, "notes %v", correct.Notes)
		require.Equal(t, prices["current19"].ID.String(), iw.held(t, schedule).Price)
	})

	t.Run("OR-J38-S35/correction-adopts-an-ended-rows-cancel-facts", func(t *testing.T) {
		iw := reconcileWorld(t, "stripe")
		iw.stripe.AddProduct("prod_basic", "Basic")
		iw.stripe.AddPrice(stripemock.Price{ID: "price_basic", Product: "prod_basic", Amount: 999, Interval: "month"})
		product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "basic", DisplayName: "Basic", Entitlements: []string{"member:basic"}})
		require.NoError(t, err)
		_, err = iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-basic", PSP: "stripe", PriceID: "price_basic"})
		require.NoError(t, err)
		start := iw.clock.Now().Add(-10 * 24 * time.Hour)
		sub := iw.stripe.AddSubscription(iw.stripe.AddCustomer(iw.alice.Email, "Alice", nil), "pm_alice", "price_basic", 999, start, start.AddDate(0, 1, 0))
		since := start.Add(-24 * time.Hour)
		iw.apply(t, iw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "stripe"}, HistorySince: &since}).ID)
		require.True(t, iw.entitled(t, customerOf(iw.alice), "member:basic"))

		// Alice cancels at Stripe. OpenRails hears two days later and records
		// the provider's end as its own: expired, that day.
		canceledAt := time.Unix(int64(iw.stripe.CancelSubscription(sub)["canceled_at"].(float64)), 0).UTC()
		iw.clock.Advance(2 * 24 * time.Hour)
		armDestructive(t, iw.authorityWorld, true)
		iw.stripe.SendWebhooksTo(iw.server.URL+"/v1/webhooks/stripe/acct_import", "whsec_import")
		_, err = iw.stripe.SendEvent(t.Context(), "customer.subscription.deleted", iw.stripe.Subscription(sub))
		require.NoError(t, err)
		settle(t, iw.fleet, kindConverge)
		cancel := func() (kind string, at time.Time) {
			require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT cancel_type, canceled_at FROM %s WHERE merchant_id = $1 AND rail_subscription_id = $2`, iw.table("subscriptions")),
				iw.alpha.MerchantID().UUID(), sub).Scan(&kind, &at))
			return kind, at.UTC()
		}
		kind, at := cancel()
		require.Equal(t, "expired", kind)
		require.False(t, at.Truncate(24*time.Hour).Equal(canceledAt.Truncate(24*time.Hour)), "recorded on another day: %s", at)
		accessColumns := func() string {
			var out string
			require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT jsonb_build_object('status', status, 'price_id', price_id, 'product_id', product_id,
				'current_period_starts_at', current_period_starts_at, 'current_period_ends_at', current_period_ends_at, 'ended_at', ended_at)::text
				FROM %s WHERE merchant_id = $1 AND rail_subscription_id = $2`, iw.table("subscriptions")), iw.alpha.MerchantID().UUID(), sub).Scan(&out))
			return out
		}
		row, access, writes := accessColumns(), iw.accessDump(t, customerOf(iw.alice)), len(iw.stripe.Mutations(""))

		imp := iw.manual(t, "stripe")
		held := subscriptionOf(t, iw, customerOf(iw.alice), sub)
		correct := proposalOf(t, iw.proposals(t, imp.ID), billing.ChangeCorrectSubscription, held.ID.String())
		require.False(t, correct.Change.Effects.EndsAccess, "cancel facts are metadata")
		require.Equal(t, "user", correct.Change.To["cancel_type"])
		require.Equal(t, canceledAt.Format(time.RFC3339), correct.Change.To["canceled_at"])
		iw.apply(t, imp.ID)
		applied, err := iw.alpha.GetFinding(t.Context(), correct.ID)
		require.NoError(t, err)
		require.NotNil(t, applied.Resolution, "Apply approves it as a correction: %s, notes %v, %v", applied.Status, deref(applied.Notes), applied.Evidence["resolution"])
		require.Equal(t, "approved", *applied.Resolution, "notes %v", deref(applied.Notes))
		kind, at = cancel()
		require.Equal(t, "user", kind, "the provider's cancel kind")
		require.True(t, at.Equal(canceledAt), "the provider's cancel date: %s", at)
		require.Equal(t, row, accessColumns(), "status, period and end stand")
		require.Equal(t, access, iw.accessDump(t, customerOf(iw.alice)), "access stays byte-identical")
		require.Len(t, iw.stripe.Mutations(""), writes, "the pull, Apply and the correction write nothing to Stripe")

		again := iw.manual(t, "stripe")
		require.Zero(t, kinds(iw.proposals(t, again.ID))[billing.ChangeCorrectSubscription], "the adopted facts agree")
	})
}

// pulled is what one import's pull proposed, as stored.
func (iw *importWorld) pulled(t *testing.T, pull billing.PSPImportID) string {
	t.Helper()
	var out string
	require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT COALESCE(jsonb_agg(jsonb_build_object('id', id, 'status', status, 'change', change,
		'digest', evidence_digest, 'updated_at', updated_at) ORDER BY id), '[]')::text FROM %s WHERE merchant_id = $1 AND pull_id = $2`, iw.table("reconciliation_findings")),
		iw.alpha.MerchantID().UUID(), pull.UUID()).Scan(&out))
	return out
}

// proposalsAll are one import's proposals in any status.
func (iw *importWorld) proposalsAll(t *testing.T, pull billing.PSPImportID) []billing.Finding {
	t.Helper()
	var out []billing.Finding
	for _, status := range []billing.FindingStatus{"reconcile_required", "requires_review", "fixed", "ignored"} {
		page, err := iw.alpha.ListProposals(t.Context(), billing.ProposalListParams{Pull: pull, Status: status, PageRequest: billing.PageRequest{Limit: 100}})
		require.NoError(t, err)
		out = append(out, page.Items...)
	}
	return out
}

// reconcileWorld is an import world without the legacy provider refresh,
// which reads the PSP on its own clock and would record what a scenario
// leaves to the reconcile.
func reconcileWorld(t *testing.T, psps ...string) *importWorld {
	t.Helper()
	iw := newImportWorld(t, psps...)
	// The fleet's scheduler queues the merchant's refresh shortly after it
	// starts; it runs before any scenario data exists or is cancelled.
	require.Eventually(t, func() bool {
		jobs, err := iw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_refresh"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.FinalizedAt == nil {
				_, err := iw.fleet.JobCancel(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		return len(jobs.Jobs) > 0
	}, 30*time.Second, 50*time.Millisecond, "the scheduler queues the merchant's refresh")
	return iw
}

// openPeriod is a 30-day member's start whose period ends tomorrow: the
// background lifecycle leaves a period alone until it lapses.
func openPeriod(iw *importWorld) time.Time {
	return iw.clock.Now().Truncate(24*time.Hour).AddDate(0, 0, -29)
}

// renewEarly is NMI billing the next period at today's midnight, a day
// before the period OpenRails holds ends: a renewal it has not seen.
func (iw *importWorld) renewEarly(ref string) {
	iw.nmi.EditSchedule(ref, func(s *nmimock.Schedule) { s.NextBilling = iw.clock.Now().Truncate(24 * time.Hour) })
	iw.nmi.RenewSchedule(ref, true)
}

// accessDump lists a customer's access windows, for a failure message.
func (iw *importWorld) accessDump(t *testing.T, customer billing.CustomerID) string {
	t.Helper()
	var out string
	require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT COALESCE(jsonb_agg(to_jsonb(a))::text, '[]') FROM %s a WHERE a.merchant_id = $1 AND a.customer_id = $2`,
		iw.table("product_access")), iw.alpha.MerchantID().UUID(), customer.UUID()).Scan(&out))
	return out
}

// bookDiff names each field of each row that differs between two books.
func bookDiff(t *testing.T, a, b string) []string {
	t.Helper()
	var before, after map[string][]map[string]any
	require.NoError(t, json.Unmarshal([]byte(a), &before))
	require.NoError(t, json.Unmarshal([]byte(b), &after))
	var out []string
	for table, rows := range after {
		was := map[string]map[string]any{}
		for _, r := range before[table] {
			was[fmt.Sprint(r["id"], r["subscription_id"], r["accepted_version"])] = r
		}
		if len(rows) != len(before[table]) {
			out = append(out, fmt.Sprintf("%s: %d rows, was %d", table, len(rows), len(before[table])))
		}
		for _, r := range rows {
			old, ok := was[fmt.Sprint(r["id"], r["subscription_id"], r["accepted_version"])]
			if !ok {
				out = append(out, fmt.Sprintf("%s: new row %v %v %v %v", table, r["id"], r["transaction_id"], r["source_type"], r["created_at"]))
				continue
			}
			for k, v := range r {
				if fmt.Sprint(v) != fmt.Sprint(old[k]) {
					out = append(out, fmt.Sprintf("%s %v.%s: %v -> %v", table, r["id"], k, old[k], v))
				}
			}
		}
	}
	return out
}

// findingDump lists every finding, for a failure message.
func (iw *importWorld) findingDump(t *testing.T) string {
	t.Helper()
	var out string
	require.NoError(t, iw.pool.QueryRow(t.Context(), fmt.Sprintf(`SELECT COALESCE(string_agg(finding_type || ' ' || status || ' ' || COALESCE(resolution, '-') || ' ' ||
		COALESCE(change->>'kind', '-') || ' ' || COALESCE(change->'to'->>'transition', '') || ' notes=' || COALESCE(operator_notes, '') || ' ' || left(COALESCE(evidence::text, ''), 120), E'\n'), '') FROM %s WHERE merchant_id = $1`,
		iw.table("reconciliation_findings")), iw.alpha.MerchantID().UUID()).Scan(&out))
	return out
}

// awaitFailed is the import once its job failed.
func (iw *importWorld) awaitFailed(t *testing.T, id billing.PSPImportID) *billing.PSPImport {
	t.Helper()
	var got *billing.PSPImport
	require.Eventually(t, func() bool {
		imp, err := iw.alpha.GetPSPImport(t.Context(), id)
		got = imp
		return err == nil && imp.Status == billing.PSPImportFailed
	}, 60*time.Second, 50*time.Millisecond)
	return got
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
