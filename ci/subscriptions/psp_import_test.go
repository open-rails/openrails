//go:build e2e && integration

package subscriptions_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/openrailstest"
)

// pspImported waits for an import's job to reach status.
func (w *world) pspImported(id billing.PSPImportID, status billing.PSPImportStatus) *billing.PSPImport {
	w.t.Helper()
	var out *billing.PSPImport
	require.Eventually(w.t, func() bool {
		var err error
		out, err = w.client[remote].GetPSPImport(w.t.Context(), id)
		require.NoError(w.t, err)
		require.NotEqual(w.t, billing.PSPImportFailed, out.Status, "import failed: %v", out.Error)
		return out.Status == status
	}, 30*time.Second, 50*time.Millisecond)
	return out
}

// pspRecords maps an import's records by ref.
func (w *world) pspRecords(id billing.PSPImportID) map[string]billing.PSPImportRecord {
	w.t.Helper()
	out := map[string]billing.PSPImportRecord{}
	cursor := ""
	for {
		page, err := w.client[remote].ListPSPImportRecords(w.t.Context(), id, billing.PSPImportRecordListParams{PageRequest: billing.PageRequest{Limit: 2, Cursor: cursor}})
		require.NoError(w.t, err)
		for _, r := range page.Items {
			out[r.Ref] = r
		}
		if page.Next == "" {
			return out
		}
		cursor = page.Next
	}
}

// A merchant's NMI subscribers import straight from the gateway: the pull
// only reads, rules and mappings link each vault, subscription and charge to
// the merchant's customers, and the linked ones land as subscriptions,
// payment methods and payments. Unmatched and conflicting objects stay staged.
func TestPSPImportFromNMI(t *testing.T) {
	t.Parallel()
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12)
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.start()
	w.waive("recorded", "the conflicted member's charge stays staged, never landed")
	ctx := t.Context()
	embeddedClient, remoteClient := w.client[embedded], w.client[remote]
	mutations := len(w.nmi.Calls())

	entitlement := "content:imported-" + uuid.NewString()[:6]
	product, err := embeddedClient.CreateProduct(ctx, billing.CreateProductParams{Key: "premium-" + uuid.NewString()[:8], DisplayName: "Premium", Entitlements: []string{entitlement}})
	require.NoError(t, err)
	plan := "premium_" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Premium", Amount: "9.99", Days: 30})

	paid := w.clock.Now().Add(10 * 24 * time.Hour)
	start := paid.AddDate(0, 0, -30)
	type member struct {
		vault, schedule string
		sale            nmimock.Sale
	}
	add := func(email, shipping, order string) member {
		vault := w.nmi.AddVault(visa)
		w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Email, v.ShippingID = email, shipping })
		schedule := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: "9.99", Days: 30, Order: order, NextBilling: paid, Created: start})
		return member{vault, schedule, w.nmi.AddScheduleSale(schedule, start)}
	}
	a, b, c, d := w.newCustomer(), w.newCustomer(), w.newCustomer(), w.newCustomer()
	directory.Put(userinfo.User{ID: a.id, Email: "a@host.test"})
	byEmail := add("A@Host.Test", "", "ord-a")        // the directory's email
	byShipping := add("", "shipment-1002", "ord-b")   // legacy user 1002
	byOrder := add("", "", "77")                      // legacy order 77
	unmatched := add("d@elsewhere.test", "", "ord-d") // nobody's
	conflicted := add("a@host.test", "shipment-1002", "ord-e")
	e := w.newCustomer()
	twice := add("", "shipment-1005", "ord-f")
	again := w.nmi.AddSchedule(nmimock.Schedule{Vault: twice.vault, Plan: plan, Amount: "9.99", Days: 30, Order: "ord-g", NextBilling: paid, Created: start})
	againSale := w.nmi.AddScheduleSale(again, start)
	oneOff := w.nmi.AddSale(nmimock.Sale{Vault: byEmail.vault, Amount: "4.99", OrderID: "one-off", At: start.Add(-5 * 24 * time.Hour)})
	partial := w.nmi.AddSale(nmimock.Sale{Vault: byEmail.vault, Amount: "4.99", OrderID: "partial-refund", At: oneOff.At})
	full := w.nmi.AddSale(nmimock.Sale{Vault: byEmail.vault, Amount: "4.99", OrderID: "full-refund", At: oneOff.At})
	failedRefund := w.nmi.AddSale(nmimock.Sale{Vault: byEmail.vault, Amount: "4.99", OrderID: "failed-refund", At: oneOff.At})
	// The sandbox v5 refund report uses separate transaction IDs with an
	// explicit original link and negative amounts. Preserve that shape here.
	w.nmi.Intercept(func(r *http.Request) bool { return true }, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
		res := serve()
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		body := string(raw)
		for _, refund := range []struct{ id, original, amount, success string }{
			{"refund-partial-1", partial.TransactionID, "-1.00", "1"},
			{"refund-partial-2", partial.TransactionID, "-1.00", "1"},
			{"refund-full", full.TransactionID, "-4.99", "1"},
			{"refund-failed", failedRefund.TransactionID, "-1.00", "0"},
		} {
			if !strings.Contains(body, "<transaction_id>"+refund.original+"</transaction_id>") {
				continue
			}
			txn := fmt.Sprintf("<transaction><transaction_id>%s</transaction_id><original_transaction_id>%s</original_transaction_id><customerid>%s</customerid><currency>USD</currency><action><amount>%s</amount><action_type>refund</action_type><date>%s</date><success>%s</success></action></transaction>", refund.id, refund.original, byEmail.vault, refund.amount, start.UTC().Format("20060102150405"), refund.success)
			body = strings.Replace(body, "</nm_response>", txn+"</nm_response>", 1)
		}
		res.Body = io.NopCloser(strings.NewReader(body))
		res.ContentLength = int64(len(body))
		res.Header.Set("Content-Length", fmt.Sprint(len(body)))
		return res, nil
	})

	res, err := embeddedClient.SetPSPImportMappings(ctx, billing.SetPSPImportMappingsParams{Mappings: []billing.PSPImportMapping{
		{Mapping: "legacy_user", Key: "1002", Customer: new(b.cid())},
		{Mapping: "legacy_order", Key: "77", Customer: new(c.cid())},
		{Mapping: "legacy_user", Key: "1005", Customer: new(e.cid())},
	}})
	require.NoError(t, err)
	require.Equal(t, 3, res.Set)

	since := start.AddDate(0, 0, -30)
	imp, err := embeddedClient.CreatePSPImport(ctx, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since,
		Rules: []billing.PSPImportRule{
			{Fact: "email", Via: billing.PSPImportViaDirectory},
			{Fact: "shipping_id", Pattern: "shipment-{key}", Via: billing.PSPImportViaMapping, Mapping: "legacy_user"},
			{Fact: "order_id", Via: billing.PSPImportViaMapping, Mapping: "legacy_order"},
		},
		Products: []billing.PSPImportProduct{{Product: product.Key}}})
	require.NoError(t, err)
	require.Equal(t, billing.PSPImportPulling, imp.Status)
	imp = w.pspImported(imp.ID, billing.PSPImportPulled)
	require.NotNil(t, imp.PulledAt)
	require.True(t, imp.HistoryThrough.Equal(*imp.PulledAt), "history is read up to the roster's read")

	recs := w.pspRecords(imp.ID)
	state := func(ref string) billing.PSPImportRecordState { return recs[ref].State }
	require.Equal(t, billing.PSPImportLinked, state(byEmail.vault))
	require.Equal(t, a.cid(), *recs[byEmail.vault].Customer)
	require.Equal(t, "rule:1", *recs[byEmail.vault].LinkedVia)
	require.Equal(t, "customer:"+byEmail.vault, *recs[byEmail.schedule].LinkedVia, "a subscription belongs to its vault's customer")
	require.Equal(t, "subscription:"+byEmail.schedule, *recs[byEmail.sale.TransactionID].LinkedVia, "a charge attributes by its order id")
	require.Equal(t, b.cid(), *recs[byShipping.vault].Customer)
	require.Equal(t, "rule:2", *recs[byShipping.vault].LinkedVia)
	require.Equal(t, c.cid(), *recs[byOrder.schedule].Customer)
	require.Equal(t, "rule:3", *recs[byOrder.schedule].LinkedVia)
	require.Equal(t, "subscription:"+byOrder.schedule, *recs[byOrder.vault].LinkedVia, "a vault takes the customer its subscriptions name")
	require.Equal(t, billing.PSPImportUnmatched, state(unmatched.vault))
	require.Equal(t, billing.PSPImportUnmatched, state(unmatched.schedule))
	require.Equal(t, billing.PSPImportConflict, state(conflicted.vault))
	require.Contains(t, *recs[conflicted.vault].Reason, a.id)
	require.Contains(t, *recs[conflicted.vault].Reason, b.id)
	require.Equal(t, billing.PSPImportConflict, state(conflicted.schedule), "what belongs to a conflicted vault is not linked")
	require.Equal(t, billing.PSPImportLinked, state(oneOff.TransactionID))
	require.Nil(t, recs[oneOff.TransactionID].SubscriptionRef, "a charge before every subscription names none")

	// The merchant links the unmatched vault by hand; what belongs to it follows.
	imp, err = remoteClient.LinkPSPImport(ctx, imp.ID, billing.LinkPSPImportParams{Links: []billing.PSPImportLink{{Record: recs[unmatched.vault].ID, Customer: new(d.cid())}}})
	require.NoError(t, err)
	recs = w.pspRecords(imp.ID)
	require.Equal(t, "manual", *recs[unmatched.vault].LinkedVia)
	require.Equal(t, d.cid(), *recs[unmatched.schedule].Customer)

	// Even an unlinked refund must stop its linked charge from landing.
	refundRef := "refund-partial-1:refund:" + start.UTC().Format(time.RFC3339)
	secondRefundRef := "refund-partial-2:refund:" + start.UTC().Format(time.RFC3339)
	require.Equal(t, billing.PSPImportLinked, recs[secondRefundRef].State)
	require.NotEqual(t, recs[refundRef].ID, recs[secondRefundRef].ID)
	require.Equal(t, billing.PSPImportLinked, recs[refundRef].State)
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.psp_import_records SET state = 'unmatched', customer_id = NULL, linked_via = NULL WHERE id IN ($1, $2)`), recs[refundRef].ID.UUID(), recs[secondRefundRef].ID.UUID())
	require.NoError(t, err)

	// Staging from older versions discarded the original ID. Hold standalone
	// history even when that refund is unmatched, until its link is qualified.
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.psp_import_records SET data = data - 'original_transaction_id' WHERE id IN ($1, $2)`), recs[refundRef].ID.UUID(), recs[secondRefundRef].ID.UUID())
	require.NoError(t, err)
	_, err = remoteClient.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	w.pspImported(imp.ID, billing.PSPImportApplied)
	held := w.pspRecords(imp.ID)
	require.Equal(t, billing.PSPImportBlocked, held[oneOff.TransactionID].State)
	require.Contains(t, *held[oneOff.TransactionID].Reason, "create a fresh import")
	for _, id := range []string{oneOff.TransactionID, partial.TransactionID, full.TransactionID} {
		payments, err := remoteClient.ListPayments(ctx, billing.PaymentListParams{CustomerID: a.cid(), TransactionID: id})
		require.NoError(t, err)
		require.Empty(t, payments.Items)
	}
	// Restore the fixture's provider-proven link and retry the held records.
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.psp_import_records SET data = jsonb_set(data, '{original_transaction_id}', to_jsonb($2::text)) WHERE id IN ($1, $3)`), recs[refundRef].ID.UUID(), partial.TransactionID, recs[secondRefundRef].ID.UUID())
	require.NoError(t, err)
	_, err = remoteClient.LinkPSPImport(ctx, imp.ID, billing.LinkPSPImportParams{})
	require.NoError(t, err)
	_, err = w.pool.Exec(ctx, w.q(`UPDATE billing.psp_import_records SET state = 'unmatched', customer_id = NULL, linked_via = NULL WHERE id IN ($1, $2)`), recs[refundRef].ID.UUID(), recs[secondRefundRef].ID.UUID())
	require.NoError(t, err)

	imp, err = remoteClient.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	imp = w.pspImported(imp.ID, billing.PSPImportApplied)

	prices, err := remoteClient.ListPrices(ctx, billing.PriceListParams{ProductID: product.ID})
	require.NoError(t, err)
	require.Len(t, prices.Items, 1, "the plan became a provider price of the product")
	require.True(t, prices.Items[0].Archived, "kept for imported members, never sold")
	require.Equal(t, int64(9_990_000), prices.Items[0].UnitAmount, "terms read from the NMI plan")

	for _, m := range []struct {
		who *customer
		row member
	}{{a, byEmail}, {b, byShipping}, {c, byOrder}, {d, unmatched}} {
		subs, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: m.who.cid()})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		sub := subs.Items[0]
		require.Equal(t, m.row.schedule, str(sub.RailSubscriptionID))
		require.Equal(t, "active", string(sub.Status))
		require.True(t, m.who.entitled(entitlement))
		methods, err := remoteClient.ListPaymentMethods(ctx, m.who.cid(), billing.PaymentMethodListParams{})
		require.NoError(t, err)
		require.Len(t, methods.Items, 1)
		require.Equal(t, "4242", *methods.Items[0].Card.Last4)
		payments, err := remoteClient.ListPayments(ctx, billing.PaymentListParams{CustomerID: m.who.cid(), TransactionID: m.row.sale.TransactionID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "the signup charge is history")
		require.Equal(t, int64(9_990_000), payments.Items[0].Amount)
	}
	recs = w.pspRecords(imp.ID)
	for _, ref := range []string{byEmail.vault, byEmail.schedule, byEmail.sale.TransactionID, byOrder.vault, unmatched.schedule} {
		require.Equal(t, billing.PSPImportImported, state(ref), ref)
	}
	require.Equal(t, billing.PSPImportImported, recs[oneOff.TransactionID].State)
	require.Equal(t, billing.PSPImportBlocked, recs[partial.TransactionID].State)
	require.Equal(t, billing.PSPImportBlocked, recs[full.TransactionID].State)
	require.Contains(t, *recs[partial.TransactionID].Reason, "recorded refund")
	require.Equal(t, billing.PSPImportUnmatched, recs[refundRef].State)
	require.Equal(t, billing.PSPImportImported, recs[failedRefund.TransactionID].State)
	for _, id := range []string{partial.TransactionID, full.TransactionID} {
		payments, err := remoteClient.ListPayments(ctx, billing.PaymentListParams{CustomerID: a.cid(), TransactionID: id})
		require.NoError(t, err)
		require.Empty(t, payments.Items, "refunded history stays staged")
	}
	history, err := remoteClient.ListPayments(ctx, billing.PaymentListParams{CustomerID: a.cid(), TransactionID: oneOff.TransactionID})
	require.NoError(t, err)
	require.Len(t, history.Items, 1)
	require.Equal(t, int64(4_990_000), history.Items[0].Amount)
	require.Nil(t, history.Items[0].SubscriptionID)
	require.Equal(t, billing.PSPImportConflict, recs[conflicted.schedule].State)

	// A customer's second live schedule of the product cannot land beside the
	// first: it alone is blocked, and the rest of the book lands.
	twins, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: e.cid()})
	require.NoError(t, err)
	require.Len(t, twins.Items, 1)
	landed, blocked, blockedSale := twice.schedule, again, againSale
	if str(twins.Items[0].RailSubscriptionID) == again {
		landed, blocked, blockedSale = again, twice.schedule, twice.sale
	}
	require.Equal(t, billing.PSPImportImported, state(landed))
	require.Equal(t, billing.PSPImportBlocked, state(blocked))
	require.Contains(t, *recs[blocked].Reason, "subscriptions_customer_id_product_id_key")
	require.Equal(t, billing.PSPImportBlocked, state(blockedSale.TransactionID))
	nobody, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{Rail: "nmi"})
	require.NoError(t, err)
	for _, s := range nobody.Items {
		require.NotEqual(t, conflicted.schedule, str(s.RailSubscriptionID), "a conflict never lands")
	}

	// Linking and landing again changes nothing.
	_, err = remoteClient.LinkPSPImport(ctx, imp.ID, billing.LinkPSPImportParams{})
	require.NoError(t, err)
	_, err = remoteClient.ApplyPSPImport(ctx, imp.ID)
	require.NoError(t, err)
	w.pspImported(imp.ID, billing.PSPImportApplied)
	subs, err := remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: a.cid()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	require.Equal(t, billing.PSPImportImported, w.pspRecords(imp.ID)[byEmail.schedule].State)

	require.Len(t, w.nmi.Calls(), mutations, "the import only read NMI")
	require.Positive(t, w.nmi.Reads()["query:recurring"])

	require.NoError(t, remoteClient.DeletePSPImport(ctx, imp.ID))
	_, err = remoteClient.GetPSPImport(ctx, imp.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	subs, err = remoteClient.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: a.cid()})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1, "what an import landed stays")
}

func TestPSPImportNMIStandaloneRefundHistory(t *testing.T) {
	t.Parallel()
	directory := &openrailstest.UserInfo{}
	w := prepareWorld(t, 12)
	w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	w.start()
	ctx, client := t.Context(), w.client[remote]
	buyer := w.newCustomer()
	directory.Put(userinfo.User{ID: buyer.id, Email: "history@host.test"})
	vault := w.nmi.AddVault(visa)
	w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Email = "history@host.test" })
	paid := w.clock.Now().Add(-5 * day)
	partial := w.nmi.AddSale(nmimock.Sale{Vault: vault, Amount: "4.23", At: paid})
	full := w.nmi.AddSale(nmimock.Sale{Vault: vault, Amount: "4.23", At: paid})
	refundedAt := paid.Add(time.Hour)
	w.nmi.Intercept(func(r *http.Request) bool { return true }, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
		res := serve()
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		body := string(raw)
		for _, r := range []struct{ id, original, amount string }{
			{"12682586568", partial.TransactionID, "-1.01"},
			{"12682589153", partial.TransactionID, "-1.01"},
			{"12682590253", full.TransactionID, "-4.23"},
		} {
			if !strings.Contains(body, "<transaction_id>"+r.original+"</transaction_id>") {
				continue
			}
			txn := fmt.Sprintf("<transaction><transaction_id>%s</transaction_id><original_transaction_id>%s</original_transaction_id><customerid>%s</customerid><currency>USD</currency><action><amount>%s</amount><action_type>refund</action_type><date>%s</date><success>1</success></action></transaction>", r.id, r.original, vault, r.amount, refundedAt.UTC().Format("20060102150405"))
			body = strings.Replace(body, "</nm_response>", txn+"</nm_response>", 1)
		}
		res.Body = io.NopCloser(strings.NewReader(body))
		res.ContentLength = int64(len(body))
		res.Header.Set("Content-Length", fmt.Sprint(len(body)))
		return res, nil
	})
	mutations := len(w.nmi.Calls())
	since := paid.Add(-day)
	for range 2 {
		// A fresh import must deduplicate against the earlier import's rows.
		imp, err := client.CreatePSPImport(ctx, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}, HistorySince: &since, Rules: []billing.PSPImportRule{{Fact: "email", Via: billing.PSPImportViaDirectory}}})
		require.NoError(t, err)
		w.pspImported(imp.ID, billing.PSPImportPulled)
		_, err = client.ApplyPSPImport(ctx, imp.ID)
		require.NoError(t, err)
		w.pspImported(imp.ID, billing.PSPImportApplied)
		recs := w.pspRecords(imp.ID)
		for _, id := range []string{partial.TransactionID, full.TransactionID, "12682586568:refund:" + refundedAt.Format(time.RFC3339), "12682589153:refund:" + refundedAt.Format(time.RFC3339), "12682590253:refund:" + refundedAt.Format(time.RFC3339)} {
			require.Equal(t, billing.PSPImportImported, recs[id].State, "%s: %v", id, recs[id].Reason)
		}
		for _, tc := range []struct {
			id    string
			total int64
		}{{partial.TransactionID, 2_020_000}, {full.TransactionID, 4_230_000}} {
			page, err := client.ListPayments(ctx, billing.PaymentListParams{CustomerID: buyer.cid(), TransactionID: tc.id})
			require.NoError(t, err)
			require.Len(t, page.Items, 1)
			require.Nil(t, page.Items[0].SubscriptionID)
			require.Equal(t, tc.total, page.Items[0].AmountRefunded)
		}
		require.Equal(t, 5, w.countRows(`SELECT count(*) FROM billing.payments`))
		require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.orders`))
	}
	for _, table := range []string{"subscriptions", "grants", "provider_intents"} {
		require.Equal(t, 0, w.countRows("SELECT count(*) FROM billing."+table), table)
	}
	require.Len(t, w.nmi.Calls(), mutations, "pull and apply do not request charges or refunds")
}
