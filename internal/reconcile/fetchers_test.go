package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// newNMITestClient points a real NMI client at a loopback fake.
func newNMITestClient(t *testing.T, h http.Handler) *nmi.NMIClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "mobius", &config.NMIProviderSettings{SecurityKey: "k", WebhookSecret: "s"}, true)
	require.NoError(t, err)
	client.LoopbackFixture = true
	client.DirectPostURL = srv.URL
	client.QueryURL = srv.URL
	client.V5BaseURL = srv.URL
	return client
}

// nmiBulkFake serves v5 roster pages (cursor = page index) and query.php
// transaction pages; anything else is a test failure.
type nmiBulkFake struct {
	subs, custs [][]string
	txnPages    []string
	v5Paths     []string
	cursors     map[string][]string
	queries     []url.Values
}

func (f *nmiBulkFake) fetch(t *testing.T, params FetchParams) *RemoteSnapshot {
	t.Helper()
	f.cursors = map[string][]string{}
	client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.v5Paths = append(f.v5Paths, r.URL.Path)
			pages := map[string][][]string{"/subscriptions": f.subs, "/customers": f.custs}[r.URL.Path]
			key := strings.TrimPrefix(r.URL.Path, "/")
			cursor := r.URL.Query().Get("cursor")
			f.cursors[key] = append(f.cursors[key], cursor)
			i, _ := strconv.Atoi(cursor)
			var items []string
			if i < len(pages) {
				items = pages[i]
			}
			next, more := "null", "false"
			if i+1 < len(pages) {
				next, more = strconv.Quote(strconv.Itoa(i+1)), "true"
			}
			fmt.Fprintf(w, `{%q:[%s],"next_cursor":%s,"has_more":%s}`, key, strings.Join(items, ","), next, more)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("report_type") != "transaction" {
			t.Errorf("only the transaction search may hit query.php: %v %v", err, r.Form)
		}
		f.queries = append(f.queries, r.Form)
		page, _ := strconv.Atoi(r.Form.Get("page_number"))
		if page >= 1 && page <= len(f.txnPages) {
			fmt.Fprint(w, f.txnPages[page-1])
			return
		}
		fmt.Fprint(w, `<?xml version="1.0"?><nm_response></nm_response>`)
	}))
	snap, err := NewNMIFetcher(client).Fetch(context.Background(), params)
	require.NoError(t, err)
	return snap
}

const nmiTransactionFixture = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response>
	<transaction>
		<transaction_id>12030573544</transaction_id><condition>pendingsettlement</condition>
		<order_id>119f105d-6f72-4257-ab53-353ed2c99e69</order_id><currency>USD</currency>
		<action><amount>23.99</amount><action_type>sale</action_type><date>20260505193542</date><success>1</success><response_code>100</response_code></action>
		<action><amount>23.99</amount><action_type>settle</action_type><date>20260506000000</date><success>1</success><response_code>100</response_code></action>
	</transaction>
	<transaction>
		<transaction_id>12030573999</transaction_id><condition>failed</condition><currency>USD</currency>
		<action><amount>9.99</amount><action_type>sale</action_type><date>20260507010203</date><success>0</success><source>recurring</source><response_text>Insufficient funds</response_text><response_code>202</response_code></action>
	</transaction>
	<transaction>
		<transaction_id>12030574000</transaction_id><condition>complete</condition><currency>USD</currency>
		<action><amount>5.00</amount><action_type>refund</action_type><date>20260508120000</date><success>1</success><response_code>100</response_code></action>
	</transaction>
</nm_response>`

func nmiTransactionPage(start, count int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><nm_response>`)
	for i := start; i < start+count; i++ {
		fmt.Fprintf(&b, `<transaction><transaction_id>txn_%04d</transaction_id><condition>complete</condition><currency>USD</currency><action><amount>1.00</amount><action_type>sale</action_type><date>20260501000000</date><success>1</success><response_code>100</response_code></action></transaction>`, i)
	}
	b.WriteString(`</nm_response>`)
	return b.String()
}

func TestNMIFetcher(t *testing.T) {
	t.Run("maps rosters, charge events and the vault", func(t *testing.T) {
		f := &nmiBulkFake{
			subs: [][]string{{
				`{"object":"subscription","id":"11494735091","customer_vault_id":"2144883496","amount":"9.99","next_billing_date":"2999-06-17","plan":{"id":"basic_monthly","plan_amount":"9.99"}}`,
				`{"object":"subscription","id":"11572482979","customer_vault_id":"987654","amount":"","next_billing_date":"2020-01-01","plan":{"id":"basic_monthly","plan_amount":"9.99"}}`,
				`{"object":"subscription","id":"p1","next_billing_date":"2999-01-01","paused_subscription":"1"}`,
			}},
			custs:    [][]string{{`{"object":"customer","id":"2144883496","billing":[{"id":"B1","priority":1,"first_name":"Rippler","last_name":"Ixas","email":"ripix@example.com","payment_details":{"card_number":"4***********1111","card_exp":"1128"}}]}`}},
			txnPages: []string{nmiTransactionFixture},
		}
		since, until := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
		snap := f.fetch(t, FetchParams{Since: since, Until: until})

		require.Equal(t, ProviderNMI, snap.Provider)
		require.False(t, snap.Capabilities.Chargebacks)
		require.True(t, snap.Coverage.CanPruneSubscriptions())
		require.True(t, snap.Coverage.CanPruneTransactions())
		require.True(t, snap.Coverage.TransactionWindowSince.Equal(since))
		require.True(t, snap.Coverage.TransactionWindowUntil.Equal(until))

		require.Len(t, snap.Subscriptions, 3)
		live, stale, paused := snap.Subscriptions[0], snap.Subscriptions[1], snap.Subscriptions[2]
		require.Equal(t, []string{string(SubscriptionStatusUnknown), "paused"}, []string{string(paused.Status), paused.RawStatus}, "paused is neither live nor dead")
		require.Equal(t, SubscriptionStatusActive, live.Status, "future next_billing_date")
		require.Equal(t, "basic_monthly", live.PlanID)
		require.Equal(t, int64(999), live.AmountCents)
		require.Equal(t, "ripix@example.com", live.Email, "joined from the vault via customer_vault_id")
		require.Equal(t, "Rippler Ixas", live.Username)
		require.Equal(t, SubscriptionStatusPastDue, stale.Status, "past next_billing_date")
		require.Equal(t, "987654", stale.CustomerID)
		require.Empty(t, stale.Email, "no vault match, no fabricated email")
		require.Equal(t, int64(999), stale.AmountCents, "empty amount falls back to the plan amount")

		require.Len(t, snap.Transactions, 3, "settle actions are not charge events")
		sale, decline, refund := snap.Transactions[0], snap.Transactions[1], snap.Transactions[2]
		require.Equal(t, RemoteTransaction{TransactionID: "12030573544", Type: TransactionTypeSale, Success: true, AmountCents: 2399, Currency: "USD",
			OccurredAt: time.Date(2026, 5, 5, 19, 35, 42, 0, time.UTC)}, withoutRaw(sale))
		require.Contains(t, string(sale.Raw), "119f105d-6f72-4257-ab53-353ed2c99e69")
		require.Equal(t, TransactionTypeDecline, decline.Type)
		require.False(t, decline.Success)
		require.Equal(t, "Insufficient funds", decline.DeclineReason)
		require.Equal(t, int64(999), decline.AmountCents)
		require.Equal(t, TransactionTypeRefund, refund.Type)
		require.Equal(t, int64(500), refund.AmountCents)

		require.Len(t, snap.PaymentMethods, 1)
		vault := snap.PaymentMethods[0]
		require.Equal(t, []string{"2144883496", "1111", "1128", "ripix@example.com"}, []string{vault.RailCustomerRef, vault.CardLast4, vault.CardExpiry, vault.Email})

		require.Equal(t, []string{"/customers", "/subscriptions"}, f.v5Paths)
		require.Len(t, f.queries, 1)
		q := f.queries[0]
		require.Equal(t, "20260501000000", q.Get("start_date"))
		require.Equal(t, "20260611000000", q.Get("end_date"))
		for _, filter := range []string{"condition", "type", "recurring"} {
			require.NotContains(t, q, filter, "declines and every sale source must be included")
		}
	})

	t.Run("paginates every list", func(t *testing.T) {
		f := &nmiBulkFake{
			subs:     [][]string{{`{"id":"s1","next_billing_date":"2999-01-01"}`}, {`{"id":"s2","next_billing_date":"2999-01-01"}`}},
			custs:    [][]string{{`{"id":"c1","billing":[]}`}, {`{"id":"c2","billing":[]}`}},
			txnPages: []string{nmiTransactionPage(0, nmiQueryPageLimit), nmiTransactionPage(nmiQueryPageLimit, 1)},
		}
		snap := f.fetch(t, FetchParams{Since: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)})
		require.Len(t, snap.Subscriptions, 2)
		require.Len(t, snap.PaymentMethods, 2)
		require.Len(t, snap.Transactions, nmiQueryPageLimit+1)
		require.Equal(t, []string{"", "1"}, f.cursors["subscriptions"])
		require.Equal(t, []string{"", "1"}, f.cursors["customers"])
		require.Len(t, f.queries, 2)
		require.True(t, snap.Coverage.TransactionsPaginatedComplete)
	})

	// #842: exhaustiveness authorizes cancelling everything absent; a 200 with
	// zero rows is indistinguishable from someone else's gateway.
	t.Run("an empty roster is never exhaustive", func(t *testing.T) {
		require.False(t, (&nmiBulkFake{}).fetch(t, FetchParams{}).Coverage.SubscriptionsExhaustive)
		full := &nmiBulkFake{subs: [][]string{{`{"id":"s1","next_billing_date":"2999-01-01"}`}}}
		require.True(t, full.fetch(t, FetchParams{}).Coverage.SubscriptionsExhaustive)
	})
}

func withoutRaw(txn RemoteTransaction) RemoteTransaction {
	txn.Raw = nil
	return txn
}

const stripeSubActive = `{"id":"sub_active1","object":"subscription","status":"active","customer":"cus_A","currency":"usd",
	"items":{"data":[{"current_period_start":1780709768,"current_period_end":1783301768,"price":{"id":"price_X","unit_amount":11900,"currency":"usd"}}]},
	"default_payment_method":{"id":"pm_1","object":"payment_method","card":{"last4":"4242","exp_month":7,"exp_year":2030}}}`

const stripeSubCanceled = `{"id":"sub_gone1","object":"subscription","status":"canceled","customer":"cus_B","currency":"usd",
	"items":{"data":[{"price":{"id":"price_Y","unit_amount":999,"currency":"usd"}}]},"default_payment_method":null}`

func stripeList(hasMore bool, items ...string) string {
	return fmt.Sprintf(`{"object":"list","has_more":%t,"data":[%s]}`, hasMore, strings.Join(items, ","))
}

// newStripeFake serves /v1/subscriptions from pages (starting_after = last id
// of the previous page) and the given event lists; it records every request.
func newStripeFake(t *testing.T, pages [][]string, events map[string]string, requests *[]*url.URL) *StripeFetcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer sk_test_x" {
			t.Errorf("unexpected %s %s auth=%q", r.Method, r.URL, r.Header.Get("Authorization"))
		}
		if requests != nil {
			*requests = append(*requests, r.URL)
		}
		if r.URL.Path == "/v1/subscriptions" {
			i := 0
			if after := r.URL.Query().Get("starting_after"); after != "" {
				i = len(pages)
				for j := range pages {
					if strings.Contains(pages[j][len(pages[j])-1], `"id":"`+after+`"`) {
						i = j + 1
					}
				}
			}
			if i >= len(pages) {
				fmt.Fprint(w, stripeList(false))
				return
			}
			fmt.Fprint(w, stripeList(i+1 < len(pages), pages[i]...))
			return
		}
		if body, ok := events[r.URL.Path]; ok {
			fmt.Fprint(w, body)
			return
		}
		fmt.Fprint(w, stripeList(false))
	}))
	t.Cleanup(srv.Close)
	return &StripeFetcher{SecretKey: "sk_test_x", BaseURL: srv.URL, HTTPClient: srv.Client()}
}

func TestStripeFetcher(t *testing.T) {
	t.Run("maps the paginated roster and windowed events", func(t *testing.T) {
		var requests []*url.URL
		events := map[string]string{
			"/v1/charges": stripeList(false,
				`{"id":"ch_ok","amount":11900,"currency":"usd","created":1780709768,"paid":true,"captured":true,"status":"succeeded","invoice":"in_1"}`,
				`{"id":"ch_fail","amount":999,"currency":"usd","created":1780809768,"paid":false,"captured":false,"status":"failed","failure_code":"card_declined","failure_message":"Your card was declined."}`),
			"/v1/refunds":  stripeList(false, `{"id":"re_1","amount":11900,"currency":"usd","created":1780909768,"status":"succeeded","charge":"ch_ok"}`),
			"/v1/disputes": stripeList(false, `{"id":"dp_1","amount":11900,"currency":"usd","created":1781009768,"status":"needs_response","charge":"ch_ok"}`),
		}
		fetcher := newStripeFake(t, [][]string{{stripeSubActive}, {stripeSubCanceled}}, events, &requests)
		since, until := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
		snap, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until})
		require.NoError(t, err)

		require.True(t, snap.Capabilities.Vault)
		require.True(t, snap.Coverage.CanPruneSubscriptions())
		require.True(t, snap.Coverage.CanPruneTransactions())

		require.Len(t, snap.Subscriptions, 2)
		active := snap.Subscriptions[0]
		require.Equal(t, "sub_active1", active.RailSubscriptionID)
		require.Equal(t, SubscriptionStatusActive, active.Status)
		require.Equal(t, "cus_A", active.CustomerID)
		require.Equal(t, "price_X", active.PlanID)
		require.Equal(t, int64(11900), active.AmountCents)
		require.Equal(t, "USD", active.Currency, "CUR-6: ingestion upper-cases")
		require.Equal(t, time.Unix(1780709768, 0).UTC(), *active.LastBilledAt)
		require.Equal(t, time.Unix(1783301768, 0).UTC(), *active.NextBillingAt)
		require.Equal(t, SubscriptionStatusCancelled, snap.Subscriptions[1].Status)
		require.Equal(t, "canceled", snap.Subscriptions[1].RawStatus)

		require.Len(t, snap.PaymentMethods, 1, "only an expanded default payment method is vault evidence")
		pm := snap.PaymentMethods[0]
		require.Equal(t, []string{"cus_A", "4242", "0730"}, []string{pm.RailCustomerRef, pm.CardLast4, pm.CardExpiry})

		var types []TransactionType
		for _, txn := range snap.Transactions {
			types = append(types, txn.Type)
		}
		require.Equal(t, []TransactionType{TransactionTypeSale, TransactionTypeDecline, TransactionTypeRefund, TransactionTypeChargeback}, types)
		require.True(t, snap.Transactions[0].Success)
		require.False(t, snap.Transactions[1].Success)
		require.Contains(t, snap.Transactions[1].DeclineReason, "card_declined")
		require.Equal(t, time.Unix(1781009768, 0).UTC(), snap.Transactions[3].OccurredAt)

		for _, u := range requests {
			q := u.Query()
			if u.Path == "/v1/subscriptions" {
				require.Equal(t, "all", q.Get("status"), "the roster includes terminal subscriptions")
				require.Empty(t, q.Get("created[gte]"), "the roster is never date-windowed")
				continue
			}
			require.Equal(t, strconv.FormatInt(since.Unix(), 10), q.Get("created[gte]"), u.Path)
			require.Equal(t, strconv.FormatInt(until.Unix(), 10), q.Get("created[lte]"), u.Path)
		}
	})

	t.Run("a single subscription is read by id", func(t *testing.T) {
		fetcher := newStripeFake(t, nil, map[string]string{"/v1/subscriptions/sub_active1": stripeSubActive}, nil)
		snap, err := fetcher.Fetch(context.Background(), FetchParams{SubscriptionID: "sub_active1"})
		require.NoError(t, err)
		require.Len(t, snap.Subscriptions, 1)
		require.Equal(t, "sub_active1", snap.Subscriptions[0].RailSubscriptionID)
	})

	// or#842: only a non-empty, merchant-wide roster proves absence.
	t.Run("exhaustiveness needs a non-empty book-wide roster", func(t *testing.T) {
		for _, c := range []struct {
			pages    [][]string
			customer string
			want     bool
		}{
			{nil, "", false},
			{[][]string{{stripeSubActive}}, "", true},
			{[][]string{{stripeSubActive}}, "cus_A", false},
		} {
			snap, err := newStripeFake(t, c.pages, nil, nil).Fetch(context.Background(), FetchParams{CustomerID: c.customer})
			require.NoError(t, err)
			require.Equal(t, c.want, snap.Coverage.SubscriptionsExhaustive, "pages=%d customer=%q", len(c.pages), c.customer)
		}
	})
}

func TestCCBillFetcher(t *testing.T) {
	var exportForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.URL.Path != "/data/main.cgi" {
			t.Errorf("unexpected DataLink request %s: %v", r.URL.Path, err)
		}
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			fmt.Fprint(w, `"ACTIVEMEMBERS","900100","0000007498","0125217202000000017","2026-05-03","user1","u1@example.com","1","2026-07-03","2026-07-03"`+"\n"+
				`"ACTIVEMEMBERS","900100","0000007498","0999000000000000009","2026-05-03","user2","u2@example.com","0","2026-07-03","2026-07-03"`+"\n")
			return
		}
		exportForm = r.Form
		fmt.Fprint(w, strings.Join([]string{
			`"REBILL","123","x","0125217202000000017","2026-06-01 04:05:06","918273645","23.99"`,
			`"CANCELLATION","123","x","0999000000000000001","2026-06-02"`,
			`"EXPIRE","123","x","0999000000000000002","2026-06-03"`,
			`"REFUND","123","x","0125217202000000017","2026-06-04","23.99"`,
			`"CHARGEBACK","123","x","0125217202000000017","2026-06-05","23.99"`,
		}, "\n"))
	}))
	t.Cleanup(srv.Close)
	fetcher := NewCCBillFetcher(&ccbill.DataLinkClient{BaseURL: srv.URL, ClientAccNum: "900100", Username: "u", Password: "p", HTTPClient: srv.Client()})

	since, until := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until})
	require.NoError(t, err)
	require.False(t, snap.Capabilities.Vault)
	require.False(t, snap.Coverage.SubscriptionsExhaustive, "CCBill absence is out-of-window, never proof")

	var names []string
	for _, typ := range ccbill.AllDataLinkTxnTypes {
		names = append(names, string(typ))
	}
	require.Equal(t, strings.Join(names, ","), exportForm.Get("transactionTypes"))
	require.Equal(t, "20260511170000", exportForm.Get("startTime"), "DataLink reads MST")
	require.Equal(t, "20260610170000", exportForm.Get("endTime"))

	require.Len(t, snap.Subscriptions, 4)
	active := snap.Subscriptions[0]
	require.Equal(t, SubscriptionStatusActive, active.Status)
	require.Equal(t, "1", active.RawStatus)
	require.Equal(t, []string{"u1@example.com", "user1", "0000007498"}, []string{active.Email, active.Username, active.PlanID})
	require.Equal(t, time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC), *active.NextBillingAt)
	require.Equal(t, SubscriptionStatusUnknown, snap.Subscriptions[1].Status, "a non-active roster flag is never guessed")
	require.Equal(t, SubscriptionStatusCancelled, snap.Subscriptions[2].Status)
	require.Equal(t, "CANCELLATION", snap.Subscriptions[2].RawStatus)
	require.Equal(t, SubscriptionStatusExpired, snap.Subscriptions[3].Status)

	require.Len(t, snap.Transactions, 3)
	rebill, refund, chargeback := snap.Transactions[0], snap.Transactions[1], snap.Transactions[2]
	require.Equal(t, RemoteTransaction{TransactionID: "918273645", SubscriptionID: "0125217202000000017", Type: TransactionTypeSale, Success: true,
		AmountCents: 2399, Currency: "USD", OccurredAt: time.Date(2026, 6, 1, 4, 5, 6, 0, time.UTC)}, withoutRaw(rebill))
	require.Equal(t, TransactionTypeRefund, refund.Type)
	require.Equal(t, int64(2399), refund.AmountCents)
	require.Empty(t, refund.TransactionID, "REFUND rows carry no transaction id")
	require.Equal(t, TransactionTypeChargeback, chargeback.Type)
	require.Contains(t, string(chargeback.Raw), "ccbill_datalink_export")

	narrowed, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until, SubscriptionID: "0125217202000000017"})
	require.NoError(t, err)
	require.Len(t, narrowed.Subscriptions, 1, "the roster has no server-side filter; narrowing is client-side")
	require.Len(t, narrowed.Transactions, 3)
	for _, txn := range narrowed.Transactions {
		require.Equal(t, "0125217202000000017", txn.SubscriptionID)
	}
}
