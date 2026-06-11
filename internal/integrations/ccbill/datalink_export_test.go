package ccbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFetchTransactionExportSendsWindowAndTypes(t *testing.T) {
	t.Parallel()

	seen := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen <- r.Form
		_, _ = w.Write([]byte(strings.Join([]string{
			`"REBILL","900000","0","0125217202000000017","2026-06-01 04:05:06","918273645","23.99"`,
			`"CANCELLATION","900000","0","0999000000000000001","2026-06-02"`,
		}, "\n")))
	}))
	t.Cleanup(server.Close)

	client := &DataLinkClient{
		BaseURL:      server.URL,
		ClientAccNum: "900000",
		ClientSubAcc: "0000",
		Username:     "user",
		Password:     "pass",
		DevMode:      true,
		HTTPClient:   server.Client(),
	}

	// 2026-06-11 00:00 UTC == 2026-06-10 17:00 MST (UTC-7).
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	rows, err := client.FetchTransactionExport(context.Background(), start, end,
		[]DataLinkTxnType{DataLinkTxnRebill, DataLinkTxnCancellation})
	require.NoError(t, err)

	form := <-seen
	require.Equal(t, "REBILL,CANCELLATION", form.Get("transactionTypes"))
	require.Equal(t, "20260531170000", form.Get("startTime"))
	require.Equal(t, "20260610170000", form.Get("endTime"))
	require.Equal(t, "900000", form.Get("clientAccnum"))
	require.Equal(t, "0000", form.Get("clientSubacc"))
	require.Equal(t, "user", form.Get("username"))
	require.Equal(t, "pass", form.Get("password"))
	require.Equal(t, "1", form.Get("testMode"))

	require.Len(t, rows, 2)
	rebill := rows[0]
	require.Equal(t, DataLinkTxnRebill, rebill.TransactionType)
	require.Equal(t, "0125217202000000017", rebill.SubscriptionID())
	require.Equal(t, "918273645", rebill.TransactionID())
	require.Equal(t, "23.99", rebill.Amount())
	require.Equal(t, "2026-06-01 04:05:06", rebill.Timestamp())

	cancel := rows[1]
	require.Equal(t, DataLinkTxnCancellation, cancel.TransactionType)
	require.Equal(t, "0999000000000000001", cancel.SubscriptionID())
	require.Empty(t, cancel.TransactionID())
	require.Empty(t, cancel.Amount())
}

func TestFetchTransactionExportEmptyWindow(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("")) // no events in window
	}))
	t.Cleanup(server.Close)

	client := &DataLinkClient{BaseURL: server.URL, ClientAccNum: "1", Username: "u", Password: "p", HTTPClient: server.Client()}
	rows, err := client.FetchTransactionExport(context.Background(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		[]DataLinkTxnType{DataLinkTxnRebill})
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestFetchTransactionExportRejectsErrorPayload(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Error: authentication failed"))
	}))
	t.Cleanup(server.Close)

	client := &DataLinkClient{BaseURL: server.URL, ClientAccNum: "1", Username: "u", Password: "p", HTTPClient: server.Client()}
	_, err := client.FetchTransactionExport(context.Background(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
		[]DataLinkTxnType{DataLinkTxnRebill})
	require.Error(t, err)
	require.Contains(t, err.Error(), "error payload")
}

func TestFetchTransactionExportValidatesArgs(t *testing.T) {
	t.Parallel()

	client := &DataLinkClient{}
	start := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	_, err := client.FetchTransactionExport(context.Background(), start, end, []DataLinkTxnType{DataLinkTxnRebill})
	require.Error(t, err)

	_, err = client.FetchTransactionExport(context.Background(), end, start, nil)
	require.Error(t, err)

	_, err = client.FetchTransactionExport(context.Background(), time.Time{}, start, []DataLinkTxnType{DataLinkTxnRebill})
	require.Error(t, err)
}
