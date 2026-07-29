package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// The transaction fixture is a trimmed copy of a live NMI sandbox query.php
// response (2026-06-11) — the transaction search stays on the classic Query
// API (#663). Subscriptions and customers are v5 JSON fixtures mirroring the
// documented resource shapes.
const nmiTransactionFixture = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response>
	<transaction>
		<transaction_id>12030573544</transaction_id>
		<condition>pendingsettlement</condition>
		<order_id>119f105d-6f72-4257-ab53-353ed2c99e69</order_id>
		<customerid>1078550313</customerid>
		<email>buyer@example.com</email>
		<currency>USD</currency>
		<action>
			<amount>23.99</amount>
			<action_type>sale</action_type>
			<date>20260505193542</date>
			<success>1</success>
			<source>api</source>
			<response_text>SUCCESS</response_text>
			<response_code>100</response_code>
		</action>
		<action>
			<amount>23.99</amount>
			<action_type>settle</action_type>
			<date>20260506000000</date>
			<success>1</success>
			<response_code>100</response_code>
		</action>
	</transaction>
	<transaction>
		<transaction_id>12030573999</transaction_id>
		<condition>failed</condition>
		<order_id>aa11105d-0000-4257-ab53-353ed2c99e70</order_id>
		<currency>USD</currency>
		<action>
			<amount>9.99</amount>
			<action_type>sale</action_type>
			<date>20260507010203</date>
			<success>0</success>
			<source>recurring</source>
			<response_text>Insufficient funds</response_text>
			<response_code>202</response_code>
		</action>
	</transaction>
	<transaction>
		<transaction_id>12030574000</transaction_id>
		<condition>complete</condition>
		<currency>USD</currency>
		<action>
			<amount>5.00</amount>
			<action_type>refund</action_type>
			<date>20260508120000</date>
			<success>1</success>
			<response_code>100</response_code>
		</action>
	</transaction>
</nm_response>`

const nmiSubscriptionsFixture = `{"subscriptions":[
  {"object":"subscription","id":"11494735091","customer_vault_id":"2144883496","amount":"9.99","next_billing_date":"2999-06-17","delayed_condition":"active","paused_subscription":"0","plan":{"object":"plan","id":"basic_monthly","plan_name":"Basic Monthly","plan_amount":"9.99","plan_payments":"0","day_frequency":"30"}},
  {"object":"subscription","id":"11572482979","customer_vault_id":"987654","amount":"","next_billing_date":"2020-01-01","delayed_condition":"active","plan":{"object":"plan","id":"basic_monthly","plan_amount":"9.99","day_frequency":"30"}}
],"next_cursor":null,"has_more":false,"per_page":100}`

const nmiCustomersFixture = `{"customers":[
  {"object":"customer","id":"2144883496","created":"2026-01-09T18:37:18.000Z","billing":[
    {"object":"billing","id":"B1","priority":1,"first_name":"Rippler","last_name":"Ixas","email":"ripix@example.com",
     "payment_details":{"card_number":"4***********1111","card_exp":"1128","card_type":"Visa"}}
  ]}
],"next_cursor":null,"has_more":false,"per_page":100}`

// newNMITestFetcher points a real *nmi.NMIClient at an httptest server that
// serves the v5 roster endpoints AND the classic query.php transaction search.
// queryRequests records the query.php form posts; v5Paths records the v5 GETs.
func newNMITestFetcher(t *testing.T, queryRequests *[]map[string]string, v5Paths *[]string) *NMIFetcher {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if v5Paths != nil {
				*v5Paths = append(*v5Paths, r.URL.Path)
			}
			switch {
			case r.URL.Path == "/subscriptions":
				_, _ = w.Write([]byte(nmiSubscriptionsFixture))
			case strings.HasPrefix(r.URL.Path, "/subscriptions/"):
				_, _ = w.Write([]byte(`{"object":"subscription","id":"` + strings.TrimPrefix(r.URL.Path, "/subscriptions/") + `","delayed_condition":"active","customer_vault_id":"2144883496","amount":"9.99","next_billing_date":"2999-06-17"}`))
			case r.URL.Path == "/customers":
				_, _ = w.Write([]byte(nmiCustomersFixture))
			default:
				t.Errorf("unexpected v5 GET %q", r.URL.Path)
			}
			return
		}
		require.NoError(t, r.ParseForm())
		seen := map[string]string{}
		for k := range r.Form {
			seen[k] = r.Form.Get(k)
		}
		if queryRequests != nil {
			*queryRequests = append(*queryRequests, seen)
		}
		require.Equal(t, "transaction", r.Form.Get("report_type"), "only the transaction search may hit query.php")
		_, _ = w.Write([]byte(nmiTransactionFixture))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL
	return NewNMIFetcher(client)
}

func TestNMIFetcher_Fetch(t *testing.T) {
	t.Parallel()

	var queryRequests []map[string]string
	var v5Paths []string
	fetcher := newNMITestFetcher(t, &queryRequests, &v5Paths)

	since := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	snap, err := fetcher.Fetch(context.Background(), FetchParams{Since: since, Until: until})
	require.NoError(t, err)

	require.Equal(t, ProviderNMI, snap.Provider)
	require.True(t, snap.Capabilities.Subscriptions)
	require.False(t, snap.Capabilities.Chargebacks)
	require.True(t, snap.Coverage.SubscriptionsExhaustive)
	require.True(t, snap.Coverage.TransactionsExhaustive)
	require.True(t, snap.Coverage.TransactionsPaginatedComplete)
	require.NotNil(t, snap.Coverage.TransactionWindowSince)
	require.NotNil(t, snap.Coverage.TransactionWindowUntil)
	require.True(t, snap.Coverage.TransactionWindowSince.Equal(since))
	require.True(t, snap.Coverage.TransactionWindowUntil.Equal(until))

	// Subscriptions: future next_billing_date => active, past => past_due.
	require.Len(t, snap.Subscriptions, 2)
	active := snap.Subscriptions[0]
	require.Equal(t, "11494735091", active.RailSubscriptionID)
	require.Equal(t, SubscriptionStatusActive, active.Status)
	require.Empty(t, active.RawStatus)
	require.Equal(t, "basic_monthly", active.PlanID)
	require.Equal(t, int64(999), active.AmountCents)
	// Email/name are joined from the customer roster via customer_vault_id.
	require.Equal(t, "ripix@example.com", active.Email)
	require.Equal(t, "Rippler Ixas", active.Username)
	require.NotNil(t, active.NextBillingAt)
	require.Contains(t, string(active.Raw), "nmi_recurring_v5")
	require.Contains(t, string(active.Raw), "11494735091")

	stale := snap.Subscriptions[1]
	require.Equal(t, SubscriptionStatusPastDue, stale.Status)
	require.Equal(t, "987654", stale.CustomerID)
	// No matching vault customer -> no email fabricated.
	require.Empty(t, stale.Email)
	// Empty subscription amount falls back to the plan amount.
	require.Equal(t, int64(999), stale.AmountCents)

	// Transactions: settle actions skipped; sale, decline, refund kept.
	require.Len(t, snap.Transactions, 3)
	sale := snap.Transactions[0]
	require.Equal(t, "12030573544", sale.TransactionID)
	require.Equal(t, TransactionTypeSale, sale.Type)
	require.True(t, sale.Success)
	require.Equal(t, int64(2399), sale.AmountCents)
	require.Equal(t, "USD", sale.Currency)
	require.Equal(t, time.Date(2026, 5, 5, 19, 35, 42, 0, time.UTC), sale.OccurredAt)
	require.Contains(t, string(sale.Raw), "119f105d-6f72-4257-ab53-353ed2c99e69")

	decline := snap.Transactions[1]
	require.Equal(t, TransactionTypeDecline, decline.Type)
	require.False(t, decline.Success)
	require.Equal(t, "Insufficient funds", decline.DeclineReason)
	require.Equal(t, int64(999), decline.AmountCents)

	refund := snap.Transactions[2]
	require.Equal(t, TransactionTypeRefund, refund.Type)
	require.True(t, refund.Success)
	require.Equal(t, int64(500), refund.AmountCents)

	// Vault entries (v5 customer roster).
	require.Len(t, snap.PaymentMethods, 1)
	vault := snap.PaymentMethods[0]
	require.Equal(t, "2144883496", vault.RailCustomerRef)
	require.Equal(t, "1111", vault.CardLast4)
	require.Equal(t, "1128", vault.CardExpiry)
	require.Equal(t, "ripix@example.com", vault.Email)

	// Request plumbing: rosters go to v5, ONLY the transaction search hits
	// query.php, with the date range forwarded and no condition filter
	// (declines must be included).
	require.Equal(t, []string{"/customers", "/subscriptions"}, v5Paths)
	require.Len(t, queryRequests, 1)
	txnReq := queryRequests[0]
	require.Equal(t, "transaction", txnReq["report_type"])
	require.Equal(t, "20260501000000", txnReq["start_date"])
	require.Equal(t, "20260611000000", txnReq["end_date"])
	require.Equal(t, "1", txnReq["page_number"])
	require.NotEmpty(t, txnReq["result_limit"])
	require.NotContains(t, txnReq, "condition")
	require.NotContains(t, txnReq, "type")      // direct-post sale marker
	require.NotContains(t, txnReq, "recurring") // add/update/delete_subscription marker
}

func TestNMIFetcher_SubscriptionFilterForwarded(t *testing.T) {
	t.Parallel()

	var v5Paths []string
	fetcher := newNMITestFetcher(t, nil, &v5Paths)

	snap, err := fetcher.Fetch(context.Background(), FetchParams{SubscriptionID: "11494735091"})
	require.NoError(t, err)
	require.Contains(t, v5Paths, "/subscriptions/11494735091")
	require.Len(t, snap.Subscriptions, 1)
	require.False(t, snap.Coverage.SubscriptionsExhaustive)
}

func TestNMIFetcher_PaginatesTransactions(t *testing.T) {
	t.Parallel()

	var txnPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/subscriptions":
				_, _ = w.Write([]byte(`{"subscriptions":[],"next_cursor":null,"has_more":false}`))
			case "/customers":
				_, _ = w.Write([]byte(`{"customers":[],"next_cursor":null,"has_more":false}`))
			default:
				t.Fatalf("unexpected v5 GET %q", r.URL.Path)
			}
			return
		}
		require.NoError(t, r.ParseForm())
		require.Equal(t, "transaction", r.Form.Get("report_type"))
		page := r.Form.Get("page_number")
		txnPages = append(txnPages, page)
		switch page {
		case "1":
			_, _ = w.Write([]byte(nmiTransactionPageXML(0, nmiQueryPageLimit)))
		case "2":
			_, _ = w.Write([]byte(nmiTransactionPageXML(nmiQueryPageLimit, 1)))
		default:
			t.Fatalf("unexpected transaction page %q", page)
		}
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL

	snap, err := NewNMIFetcher(client).Fetch(context.Background(), FetchParams{
		Since: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Len(t, snap.Transactions, nmiQueryPageLimit+1)
	require.Equal(t, []string{"1", "2"}, txnPages)
	require.True(t, snap.Coverage.TransactionsPaginatedComplete)
}

func TestNMIFetcher_PaginatesV5Rosters(t *testing.T) {
	t.Parallel()

	var subCursors, custCursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			require.NoError(t, r.ParseForm())
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
			return
		}
		cursor := r.URL.Query().Get("cursor")
		switch r.URL.Path {
		case "/subscriptions":
			subCursors = append(subCursors, cursor)
			if cursor == "" {
				_, _ = w.Write([]byte(`{"subscriptions":[{"object":"subscription","id":"s1","next_billing_date":"2999-01-01"}],"next_cursor":"42","has_more":true}`))
			} else {
				require.Equal(t, "42", cursor)
				_, _ = w.Write([]byte(`{"subscriptions":[{"object":"subscription","id":"s2","next_billing_date":"2999-01-01"}],"next_cursor":null,"has_more":false}`))
			}
		case "/customers":
			custCursors = append(custCursors, cursor)
			if cursor == "" {
				_, _ = w.Write([]byte(`{"customers":[{"object":"customer","id":"c1","billing":[]}],"next_cursor":"7","has_more":true}`))
			} else {
				require.Equal(t, "7", cursor)
				_, _ = w.Write([]byte(`{"customers":[{"object":"customer","id":"c2","billing":[]}],"next_cursor":null,"has_more":false}`))
			}
		default:
			t.Fatalf("unexpected v5 GET %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL

	snap, err := NewNMIFetcher(client).Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Len(t, snap.Subscriptions, 2)
	require.Len(t, snap.PaymentMethods, 2)
	require.Equal(t, []string{"", "42"}, subCursors)
	require.Equal(t, []string{"", "7"}, custCursors)
}

func nmiTransactionPageXML(start, count int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><nm_response>`)
	for i := 0; i < count; i++ {
		id := start + i
		fmt.Fprintf(&b, `<transaction><transaction_id>txn_%04d</transaction_id><condition>complete</condition><currency>USD</currency><action><amount>1.00</amount><action_type>sale</action_type><date>20260501000000</date><success>1</success><response_code>100</response_code></action></transaction>`, id)
	}
	b.WriteString(`</nm_response>`)
	return b.String()
}

func TestNMIFetcher_ErrorResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"authenticationError","error_code":"E_AUTH","message":"Invalid Security Key"}`))
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><error_response>Invalid Security Key</error_response></nm_response>`))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "bad"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL
	client.V5BaseURL = server.URL

	_, err = NewNMIFetcher(client).Fetch(context.Background(), FetchParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid Security Key")
}

func TestParseAmountCents(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want int64
		err  bool
	}{
		{"9.99", 999, false},
		{"23.99", 2399, false},
		{"0.00", 0, false},
		{"", 0, false},
		{"23", 2300, false},
		{"5.5", 550, false},
		{"-5.00", -500, false},
		{".99", 99, false},
		{"1.999", 0, true},
		{"abc", 0, true},
	} {
		got, err := parseAmountCents(tc.in)
		if tc.err {
			require.Error(t, err, "input %q", tc.in)
			continue
		}
		require.NoError(t, err, "input %q", tc.in)
		require.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

func TestCardLast4(t *testing.T) {
	t.Parallel()

	require.Equal(t, "1111", cardLast4("4xxxxxxxxxxx1111"))
	require.Equal(t, "", cardLast4(""))
	require.Equal(t, "", cardLast4("xxxx"))
}

// #842: SubscriptionsExhaustive is an ABSENCE PROOF — it authorizes cancelling
// every local subscription missing from the roster. A 200 with zero rows is
// indistinguishable from a complete roster of a gateway that is not ours (a
// misdeclared account_id, a credential rotated onto a sibling sub-account, an
// incident returning an empty first page with has_more=false). It must never
// be stamped exhaustive.
func TestNMIFetcher_EmptyRosterIsNotExhaustive(t *testing.T) {
	t.Parallel()

	newFetcher := func(t *testing.T, subsJSON string) *NMIFetcher {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				require.NoError(t, r.ParseForm())
				_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
				return
			}
			switch r.URL.Path {
			case "/subscriptions":
				_, _ = w.Write([]byte(subsJSON))
			case "/customers":
				_, _ = w.Write([]byte(`{"customers":[],"next_cursor":null,"has_more":false}`))
			default:
				t.Fatalf("unexpected v5 GET %q", r.URL.Path)
			}
		}))
		t.Cleanup(server.Close)
		client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
		require.NoError(t, err)
		client.QueryURL = server.URL
		client.V5BaseURL = server.URL
		return NewNMIFetcher(client)
	}

	empty, err := newFetcher(t, `{"subscriptions":[],"next_cursor":null,"has_more":false}`).
		Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.Empty(t, empty.Subscriptions)
	require.False(t, empty.Coverage.SubscriptionsExhaustive,
		"an empty roster was stamped exhaustive — it would cancel the merchant's entire book")

	full, err := newFetcher(t, `{"subscriptions":[{"object":"subscription","id":"s1","next_billing_date":"2999-01-01"}],"next_cursor":null,"has_more":false}`).
		Fetch(context.Background(), FetchParams{})
	require.NoError(t, err)
	require.True(t, full.Coverage.SubscriptionsExhaustive, "a non-empty completed roster still proves absence")
}
