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

// Fixtures are trimmed copies of live NMI sandbox query.php responses
// (2026-06-11).
const nmiRecurringFixture = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response><subscription id="11494735091"><subscription_id>11494735091</subscription_id><plan><plan_id>basic_monthly</plan_id><plan_name>Basic Monthly</plan_name><plan_amount>9.99</plan_amount><plan_payments>until_canceled</plan_payments><day_frequency>30</day_frequency></plan><next_charge_date>2999-06-17</next_charge_date><completed_payments>0</completed_payments><attempted_payments>0</attempted_payments><remaining_payments>until_canceled</remaining_payments><orderid>c2882479-a07d-4f42-980d-895e90ea4060</orderid><first_name>Rippler</first_name><last_name>Ixas</last_name><email>ripix@example.com</email><cc_number>4xxxxxxxxxxx1111</cc_number><cc_exp>1030</cc_exp></subscription><subscription id="11572482979"><subscription_id>11572482979</subscription_id><plan><plan_id>basic_monthly</plan_id><plan_amount>9.99</plan_amount><day_frequency>30</day_frequency></plan><next_charge_date>2020-01-01</next_charge_date><email>stale@example.com</email><customer_vault_id>987654</customer_vault_id></subscription></nm_response>`

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

const nmiVaultFixture = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response><customer_vault><customer id="2144883496"><first_name>Cheikh</first_name><last_name>Seck</last_name><email>vault@example.com</email><cc_number>4xxxxxxxxxxx1111</cc_number><cc_exp>1128</cc_exp><cc_type>Visa</cc_type><created>20260109183718</created><customer_vault_id>2144883496</customer_vault_id></customer></customer_vault></nm_response>`

// newNMITestFetcher points a real *nmi.NMIClient at an httptest query server
// that routes on report_type.
func newNMITestFetcher(t *testing.T, requests *[]map[string]string) *NMIFetcher {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen := map[string]string{}
		for k := range r.Form {
			seen[k] = r.Form.Get(k)
		}
		if requests != nil {
			*requests = append(*requests, seen)
		}
		switch r.Form.Get("report_type") {
		case "recurring":
			_, _ = w.Write([]byte(nmiRecurringFixture))
		case "transaction":
			_, _ = w.Write([]byte(nmiTransactionFixture))
		case "customer_vault":
			_, _ = w.Write([]byte(nmiVaultFixture))
		default:
			t.Errorf("unexpected report_type %q", r.Form.Get("report_type"))
		}
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL
	return NewNMIFetcher(client)
}

func TestNMIFetcher_Fetch(t *testing.T) {
	t.Parallel()

	var requests []map[string]string
	fetcher := newNMITestFetcher(t, &requests)

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

	// Subscriptions: future next_charge_date => active, past => past_due.
	require.Len(t, snap.Subscriptions, 2)
	active := snap.Subscriptions[0]
	require.Equal(t, "11494735091", active.ProcessorSubscriptionID)
	require.Equal(t, SubscriptionStatusActive, active.Status)
	require.Empty(t, active.RawStatus)
	require.Equal(t, "basic_monthly", active.PlanID)
	require.Equal(t, int64(999), active.AmountCents)
	require.Equal(t, "ripix@example.com", active.Email)
	require.NotNil(t, active.NextBillingAt)
	require.Contains(t, string(active.Raw), "nmi_recurring")
	require.Contains(t, string(active.Raw), "subscription_id")

	stale := snap.Subscriptions[1]
	require.Equal(t, SubscriptionStatusPastDue, stale.Status)
	require.Equal(t, "987654", stale.CustomerID)

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

	// Vault entries.
	require.Len(t, snap.VaultEntries, 1)
	vault := snap.VaultEntries[0]
	require.Equal(t, "2144883496", vault.CustomerVaultID)
	require.Equal(t, "1111", vault.CardLast4)
	require.Equal(t, "1128", vault.CardExpiry)
	require.Equal(t, "vault@example.com", vault.Email)

	// Request plumbing: date range forwarded to the transaction query; no
	// mutation-looking params anywhere; page_number=0 never emitted.
	require.Len(t, requests, 3)
	for _, req := range requests {
		require.NotContains(t, req, "type")      // direct-post sale marker
		require.NotContains(t, req, "recurring") // add/update/delete_subscription marker
		require.NotEqual(t, "0", req["page_number"])
	}
	txnReq := requests[1]
	require.Equal(t, "20260501000000", txnReq["start_date"])
	require.Equal(t, "20260611000000", txnReq["end_date"])
	require.Equal(t, "1", txnReq["page_number"])
	require.NotEmpty(t, txnReq["result_limit"])
	// No condition filter: declines must be included.
	require.NotContains(t, txnReq, "condition")
}

func TestNMIFetcher_SubscriptionFilterForwarded(t *testing.T) {
	t.Parallel()

	var requests []map[string]string
	fetcher := newNMITestFetcher(t, &requests)

	_, err := fetcher.Fetch(context.Background(), FetchParams{SubscriptionID: "11494735091"})
	require.NoError(t, err)
	require.Equal(t, "11494735091", requests[0]["subscription_id"])
}

func TestNMIFetcher_PaginatesTransactions(t *testing.T) {
	t.Parallel()

	var txnPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		switch r.Form.Get("report_type") {
		case "recurring":
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
		case "transaction":
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
		case "customer_vault":
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><customer_vault></customer_vault></nm_response>`))
		default:
			t.Fatalf("unexpected report_type %q", r.Form.Get("report_type"))
		}
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "test-key"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL

	snap, err := NewNMIFetcher(client).Fetch(context.Background(), FetchParams{
		Since: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	require.Len(t, snap.Transactions, nmiQueryPageLimit+1)
	require.Equal(t, []string{"1", "2"}, txnPages)
	require.True(t, snap.Coverage.TransactionsPaginatedComplete)
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
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><error_response>Invalid Security Key</error_response></nm_response>`))
	}))
	t.Cleanup(server.Close)

	client, err := nmi.NewClient("mobius", &config.NMIProviderSettings{SecurityKey: "bad"}, true)
	require.NoError(t, err)
	client.QueryURL = server.URL

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
