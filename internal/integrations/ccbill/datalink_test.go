package ccbill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
