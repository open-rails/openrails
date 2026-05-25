package ccbill

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDataLinkClientFetchActiveMembersSendsTestMode(t *testing.T) {
	t.Parallel()

	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen <- r.Form.Get("testMode")
		_, _ = w.Write([]byte(`"ACTIVEMEMBERS","123","x","456","2026-05-03","user","u@example.com","1","2026-06-03","2026-06-03"`))
	}))
	t.Cleanup(server.Close)

	client := &DataLinkClient{
		BaseURL:      server.URL,
		ClientAccNum: "123",
		Username:     "user",
		Password:     "pass",
		DevMode:      true,
		HTTPClient:   server.Client(),
	}

	records, err := client.FetchActiveMembers(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "1", <-seen)
}

func TestDataLinkClientProcessCSVDataFailsOnSkippedRows(t *testing.T) {
	t.Parallel()

	client := &DataLinkClient{}
	records, err := client.ProcessCSVData(context.Background(), strings.Join([]string{
		`"ACTIVEMEMBERS","123","x","456","2026-05-03","user","u@example.com","1","2026-06-03","2026-06-03"`,
		`"ACTIVEMEMBERS","123"`,
	}, "\n"))

	require.Error(t, err)
	require.Nil(t, records)
	require.Contains(t, err.Error(), "skipped records")
}

func TestDataLinkClientPreservesLeadingZeroSubscriptionID(t *testing.T) {
	t.Parallel()

	client := &DataLinkClient{}
	records, err := client.ProcessCSVData(context.Background(), strings.Join([]string{
		`"ACTIVEMEMBERS","123","x","0125217202000000017","2026-05-03","user","u@example.com","1","2026-06-03","2026-06-03"`,
	}, "\n"))

	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "0125217202000000017", records[0].SubscriptionID)
}

func TestDataLinkClientProcessCSVDataRejectsEmptySubscriptionID(t *testing.T) {
	t.Parallel()

	client := &DataLinkClient{}
	records, err := client.ProcessCSVData(context.Background(), `"ACTIVEMEMBERS","123","x","","2026-05-03","user","u@example.com","1","2026-06-03","2026-06-03"`)

	require.Error(t, err)
	require.Nil(t, records)
	require.Contains(t, err.Error(), "skipped records")
}

func TestIsDataLinkActiveStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"1", "Y", "yes", "ACTIVE", "a"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			require.True(t, IsDataLinkActiveStatus(status))
		})
	}
	for _, status := range []string{"0", "cancelled", "", "inactive"} {
		status := status
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			require.False(t, IsDataLinkActiveStatus(status))
		})
	}
}

func TestDataLinkPaidThrough(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	paidThrough, ok := DataLinkPaidThrough(CCBillRecord{ExpiryDate: "2026-06-03"}, now)
	require.True(t, ok)
	require.Equal(t, time.Date(2026, 6, 3, 23, 59, 59, 0, time.UTC), paidThrough)

	_, ok = DataLinkPaidThrough(CCBillRecord{ExpiryDate: "2026-05-01", RebillDate: ""}, now)
	require.False(t, ok)
}

func TestSleepWithContextReturnsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepWithContext(ctx, time.Hour)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
}
