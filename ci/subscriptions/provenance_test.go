//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/booktag"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/writeposture"
)

// bookID is this world's billing book, the identity every provider object it
// creates carries.
func (w *world) bookID() string {
	w.t.Helper()
	var id string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT book_id::text FROM billing.book_identity`)).Scan(&id))
	return id
}

// tagFrom moves the moment the book began tagging, before anything reads it.
func (w *world) tagFrom(at time.Time) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.book_identity SET tagging_started_at = $1`), at)
	require.NoError(w.t, err)
	booktag.Forget(w.client[embedded].MerchantID().UUID())
}

// setLive makes the rail's PSP count as one that moves real money.
func (w *world) setLive(rail string, live bool) {
	w.t.Helper()
	environment := map[bool]string{false: "sandbox", true: "live"}[live]
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.psps SET environment = $2 WHERE id = $1`), w.psp[rail].UUID(), environment)
	require.NoError(w.t, err)
}

type findingView struct {
	severity, provenance string
	notified, quiet      bool
}

func (w *world) findingView(findingType, subject string) findingView {
	w.t.Helper()
	var v findingView
	// A refresh reads the book's history window by window; the finding appears
	// with the window that holds its object.
	require.Eventually(w.t, func() bool {
		err := w.pool.QueryRow(w.t.Context(), w.q(`SELECT severity, coalesce(evidence->>'provenance', ''), notified_at IS NOT NULL, coalesce(evidence @> '{"quiet_foreign":true}', false)
			FROM billing.reconciliation_findings WHERE finding_type = $1 AND subject_key = $2`), findingType, subject).Scan(&v.severity, &v.provenance, &v.notified, &v.quiet)
		return err == nil
	}, 30*time.Second, 50*time.Millisecond, "%s %s", findingType, subject)
	return v
}

// enrollBeforeTagging enrolls an NMI member and returns when the book began
// tagging: a day after enrolment, with the world ten days on, so a charge at
// preAt predates it and one at postAt follows it. Both fall in the first
// window a refresh reads, which a held PSP never moves past.
func (w *world) enrollBeforeTagging(t *testing.T) (e *engineCase, preAt, postAt time.Time) {
	t.Helper()
	enrolled := w.clock.Now()
	start := enrolled.Add(day)
	w.tagFrom(start)
	e = enroll(t, w, "nmi", embedded)
	w.advance(10 * day)
	return e, enrolled.Add(12 * time.Hour), enrolled.Add(3 * day)
}

// foreignObjects puts a schedule and a charge on the gateway that nothing of
// the merchant names, made by carried (a book id, or "" for none) at at.
func (w *world) foreignObjects(carried string, at time.Time) (schedule, charge string) {
	vault := w.nmi.AddVault(visa)
	schedule = w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "another_environment_plan", Amount: "4.99", NextBilling: w.clock.Now().Add(10 * day), Book: carried, Created: at})
	sale := w.nmi.AddSale(nmimock.Sale{Vault: w.nmi.AddVault(visa), Amount: "5.00", Book: carried, At: at})
	return schedule, sale.TransactionID
}

// An unknown provider object is lost when this book created it, and the book
// id says so on every PSP: critical, alerted, and it holds provider recovery.
// One made by another book, or by hand after tagging began, is foreign on
// every PSP, and one that predates tagging is foreign only where no money
// moves: low severity, no alert, and recovery stays open.
func TestUnknownProviderObjectsAreLostOrForeignByTheirBook(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		carried  func(w *world) string
		pre      bool
		quietOn  map[bool]bool
		wantProv map[bool]string
	}{
		{"this book's", func(w *world) string { return w.bookID() }, false, map[bool]bool{false: false, true: false}, map[bool]string{false: "lost", true: "lost"}},
		{"another book's", func(*world) string { return uuid.NewString() }, false, map[bool]bool{false: true, true: true}, map[bool]string{false: "foreign", true: "foreign"}},
		{"untagged after tagging began", func(*world) string { return "" }, false, map[bool]bool{false: true, true: true}, map[bool]string{false: "foreign", true: "foreign"}},
		{"untagged before tagging began", func(*world) string { return "" }, true, map[bool]bool{false: true, true: false}, map[bool]string{false: "pre_tag", true: "pre_tag"}},
	}
	for _, tc := range cases {
		for _, live := range []bool{false} {
			name := tc.name + " on a sandbox PSP"
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				e, preAt, postAt := w.enrollBeforeTagging(t)
				at := postAt
				if tc.pre {
					at = preAt
				}
				schedule, charge := w.foreignObjects(tc.carried(w), at)
				if !tc.quietOn[false] {
					w.waive("recorded", "the charge this book lost stays unrecorded until an operator resolves it")
				}
				w.refreshProviders()

				quiet := tc.quietOn[live]
				sub, txn := w.findingView("pull.subscription.missing", schedule), w.findingView("pull.charge.missing", charge)
				for _, v := range []findingView{sub, txn} {
					require.Equal(t, tc.wantProv[live], v.provenance)
					require.Equal(t, quiet, v.quiet)
					require.Equal(t, !quiet, v.notified, "only what the book lost alerts an operator")
				}
				if quiet {
					require.Equal(t, "low", sub.severity)
					require.Equal(t, "low", txn.severity)
					require.NoError(t, w.recoveryCheck("nmi"), "recovery stays open")
					before := len(e.providerLedger())
					e.toFreshPeriodEnd()
					w.runRenewals()
					require.Len(t, e.providerLedger(), before+1, "the renewal is charged")
				} else {
					require.Equal(t, "critical", sub.severity)
					require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending, "recovery is held")
				}
			})
		}
	}
}

// A record that names the object keeps its full handling on a sandbox PSP,
// whoever the book on the object says made it.
func TestRecordedProviderObjectHoldsRecoveryOnSandbox(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e, _, _ := w.enrollBeforeTagging(t)
	mine := w.nmi.AddSchedule(nmimock.Schedule{Vault: w.vaultOf(e.method), Plan: "unlinked_plan", Amount: "9.99", NextBilling: w.clock.Now().Add(10 * day), Book: uuid.NewString()})
	w.refreshProviders()
	v := w.findingView("pull.subscription.missing", mine)
	require.Equal(t, []any{"critical", false, true}, []any{v.severity, v.quiet, v.notified})
	require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending)
}

// Every object OpenRails creates at NMI and Stripe carries the book id.
func TestProviderObjectsOpenRailsCreatesCarryTheBook(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	book := w.bookID()
	nmiCase := enroll(t, w, "nmi", embedded)
	require.NotEmpty(t, w.nmi.Sales())
	for _, s := range w.nmi.Sales() {
		require.Equal(t, book, s.Book, "sale %s", s.TransactionID)
	}
	_ = nmiCase
	enroll(t, w, "stripe", embedded)
	tagged := 0
	for _, call := range w.stripe.Mutations("/v1/payment_intents") {
		require.Equal(t, book, call.Form.Get("metadata[openrails_book]"), "%s", call.Path)
		tagged++
	}
	require.NotZero(t, tagged)
}

// A restored book keeps its identity: arming the copy leaves the book id, so
// an object it created and the database lost is lost, and another deployment's
// is foreign.
func TestRestoredBookRecognisesItsOwnLostObjects(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	book := w.bookID()
	e, _, postAt := w.enrollBeforeTagging(t)
	other := newWorld(t)
	require.NotEqual(t, book, other.bookID(), "every deployment is its own book")

	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.book_identity SET system_identifier = '0'`))
	require.NoError(t, err)
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	require.NoError(t, writeposture.ArmBook(t.Context(), database.GenDirectory(), "e2e restore", w.clock.Now()))
	require.Equal(t, book, w.bookID(), "arming keeps the book id")

	w.waive("recorded", "the charge this book lost stays unrecorded until an operator resolves it")
	lostSchedule, lostCharge := w.foreignObjects(book, postAt)
	otherSchedule, otherCharge := w.foreignObjects(other.bookID(), postAt)
	w.refreshProviders()
	for _, id := range []string{lostSchedule, lostCharge} {
		typ := "pull.subscription.missing"
		if id == lostCharge {
			typ = "pull.charge.missing"
		}
		v := w.findingView(typ, id)
		require.Equal(t, []any{"lost", false, true}, []any{v.provenance, v.quiet, v.notified})
	}
	for _, id := range []string{otherSchedule, otherCharge} {
		typ := "pull.subscription.missing"
		if id == otherCharge {
			typ = "pull.charge.missing"
		}
		v := w.findingView(typ, id)
		require.Equal(t, []any{"foreign", true, false}, []any{v.provenance, v.quiet, v.notified})
	}
	require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending, "what the book lost holds recovery")
	_ = e
}

// A webhook about an unknown refund answers 200 and is recorded quietly when
// this book did not create it; one this book created keeps failing, so NMI
// redelivers it.
func TestUnknownRefundWebhookByProvenance(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	refund := func(book string) obj {
		body := obj{"transaction_id": "refund-" + uuid.NewString()[:8], "transaction_type": "cc", "condition": "complete", "amount": "1.00", "currency": "USD",
			"action":      obj{"action_type": "refund", "amount": "1.00", "success": "1"},
			"transaction": obj{"transaction_id": "sale-" + uuid.NewString()[:8]}}
		if book != "" {
			body["merchant_defined_fields"] = obj{"20": book}
		}
		return nmiEvent("transaction.refund.success", body)
	}
	require.Equal(t, http.StatusOK, w.deliver("nmi", refund(uuid.NewString())), "another book's refund")
	require.Equal(t, http.StatusOK, w.deliver("nmi", refund("")), "an untagged refund on a sandbox PSP")
	require.NotEqual(t, http.StatusOK, w.deliver("nmi", refund(w.bookID())), "this book's lost refund is retried")
}

// A Stripe subscription another book made is recorded at low severity.
func TestForeignStripeSubscriptionIsInformational(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	enroll(t, w, "stripe", embedded)
	w.waive("recorded", "the foreign subscription's charge is not this book's")
	now := w.clock.Now()
	foreignSub := w.stripe.AddSubscription("cus_other_deployment", "pm_other_deployment", "price_other_deployment", 999, now.Add(-day), now.Add(29*day))
	w.stripe.TagSubscription(foreignSub, "openrails_book", uuid.NewString(), now.Add(-day))
	w.refreshProviders()
	v := w.findingView("pull.subscription.missing", foreignSub)
	require.Equal(t, []any{"low", "foreign", true, false}, []any{v.severity, v.provenance, v.quiet, v.notified})
	require.NoError(t, w.recoveryCheck("stripe"))
}

// catalogPull runs the catalog reconciliation pass and waits for it.
func (w *world) catalogPull() {
	w.t.Helper()
	_, err := w.jobs.Insert(w.t.Context(), riverjobs.CatalogReconciliationPullArgs{MerchantID: w.client[embedded].MerchantID().UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	require.Eventually(w.t, func() bool {
		page, err := w.jobs.JobList(w.t.Context(), river.NewJobListParams().Kinds(riverjobs.KindCatalogReconciliationPull).First(10))
		require.NoError(w.t, err)
		for _, job := range page.Jobs {
			if job.State == rivertype.JobStateRetryable || job.State == rivertype.JobStateDiscarded {
				w.t.Fatalf("catalog reconciliation failed: %v", job.Errors)
			}
			if job.State == rivertype.JobStateCompleted {
				return true
			}
		}
		return false
	}, 30*time.Second, 50*time.Millisecond, "the catalog pass completes")
}

// A plan on the gateway that no price names is informational on a sandbox
// gateway: low severity, no alert, and it never holds recovery. A live
// gateway's is unchanged.
func TestOrphanPlanOnSandboxIsInformational(t *testing.T) {
	t.Parallel()
	for _, live := range []bool{false} {
		t.Run("sandbox", func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12)
			w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe") }
			w.start()
			enroll(t, w, "nmi", embedded)
			w.nmi.AddPlan(nmimock.Plan{ID: "another_environment_plan", Name: "elsewhere", Amount: "4.99", Days: 30})
			w.catalogPull()
			var severity string
			var notified, quiet bool
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT severity, notified_at IS NOT NULL, coalesce(evidence @> '{"quiet_foreign":true}', false)
				FROM billing.reconciliation_findings WHERE finding_type = 'catalog.orphan_in_nmi' AND external_resource_id = 'another_environment_plan'`)).Scan(&severity, &notified, &quiet))
			require.Equal(t, []any{"low", false, !live}, []any{severity, notified, quiet})
			require.NoError(t, w.recoveryCheck("nmi"))
		})
	}
}

// fetcher answers one provider read with a fixed snapshot.
type fetcher struct{ snap reconcile.RemoteSnapshot }

func (fetcher) Name() string                         { return "nmi" }
func (fetcher) Capabilities() reconcile.Capabilities { return (&reconcile.NMIFetcher{}).Capabilities() }
func (f fetcher) Fetch(_ context.Context, p reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	snap := f.snap
	snap.FetchedAt, snap.Capabilities = p.Until, f.Capabilities()
	return &snap, nil
}

// On a PSP that moves real money an unknown object is handled by its book
// alone: lost holds recovery and alerts as always, foreign never does, and a
// pre-tag one is today's behaviour. A live PSP cannot arm against the fake
// gateway, so the engine reads a fixed provider snapshot against the real
// database, findings and recovery gate.
func TestLiveProviderObjectsByProvenance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		carried func(*world) string
		pre     bool
		quiet   bool
		want    string
	}{
		{"this book's", func(w *world) string { return w.bookID() }, false, false, "lost"},
		{"another book's", func(*world) string { return uuid.NewString() }, false, true, "foreign"},
		{"untagged after tagging began", func(*world) string { return "" }, false, true, "foreign"},
		{"untagged before tagging began", func(*world) string { return "" }, true, false, "pre_tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			_, preAt, postAt := w.enrollBeforeTagging(t)
			at := postAt
			if tc.pre {
				at = preAt
			}
			carried := tc.carried(w)
			w.refreshProviders() // recovery's applied coverage is complete before any unknown object appears
			snap := reconcile.RemoteSnapshot{Provider: reconcile.ProviderNMI,
				Subscriptions: []reconcile.RemoteSubscription{{RailSubscriptionID: "rsub_elsewhere", Status: reconcile.SubscriptionStatusActive, CustomerID: "vault_elsewhere", PlanID: "plan_elsewhere", AmountCents: 499, BookID: carried, CreatedAt: at}},
				Transactions: []reconcile.RemoteTransaction{{TransactionID: "txn_elsewhere", Type: reconcile.TransactionTypeSale, Success: true, AmountCents: 500, Currency: "USD", OccurredAt: at, BookID: carried,
					Raw: json.RawMessage(`{"customer_vault_id":"vault_elsewhere"}`)}}}
			w.setLive("nmi", true)
			defer w.setLive("nmi", false)
			database, err := db.NewWithPGXPool(w.pool, w.schema)
			require.NoError(t, err)
			engine := reconcile.NewEngine(database, &config.Config{}, nil, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: fetcher{snap}}, nil)
			mid := w.client[embedded].MerchantID()
			_, err = engine.Run(merchant.WithID(t.Context(), mid), reconcile.RunParams{Mode: reconcile.ModeAdvisory, Providers: []reconcile.Provider{reconcile.ProviderNMI},
				PSPs:  map[reconcile.Provider]reconcile.PSPBinding{reconcile.ProviderNMI: {ID: w.psp["nmi"].UUID(), Rail: "nmi", AccountID: nmiAcct}},
				Since: w.clock.Now().Add(-30 * day), Until: w.clock.Now()})
			require.NoError(t, err)

			sub, txn := w.findingView("pull.subscription.missing", "rsub_elsewhere"), w.findingView("pull.charge.missing", "txn_elsewhere")
			for _, v := range []findingView{sub, txn} {
				require.Equal(t, []any{tc.want, tc.quiet}, []any{v.provenance, v.quiet})
			}
			if tc.quiet {
				require.Equal(t, []string{"low", "low"}, []string{sub.severity, txn.severity})
				require.NoError(t, w.recoveryCheck("nmi"), "a foreign object never holds recovery")
			} else {
				require.Equal(t, "critical", sub.severity)
				require.ErrorIs(t, w.recoveryCheck("nmi"), providerrecovery.ErrPending, "recovery is held")
			}
		})
	}
}
