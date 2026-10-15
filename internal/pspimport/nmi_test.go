package pspimport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func TestNMIHistoryPreservesRefundOriginal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, original, saleSuccess, amount, wantOriginal string
	}{
		{name: "separate partial refund", original: "sale-1", amount: "-1.01", wantOriginal: "sale-1"},
		{name: "separate full refund", original: "sale-1", amount: "-4.23", wantOriginal: "sale-1"},
		{name: "combined partial refund", saleSuccess: "1", amount: "1.01", wantOriginal: "reported-1"},
		{name: "combined full refund", saleSuccess: "1", amount: "4.23", wantOriginal: "reported-1"},
		{name: "refund without original", amount: "-1.01"},
		{name: "failed sale does not prove original", saleSuccess: "0", amount: "-1.01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sale := ""
			if tc.saleSuccess != "" {
				sale = fmt.Sprintf(`<action><amount>4.23</amount><action_type>sale</action_type><date>20261015080000</date><success>%s</success></action>`, tc.saleSuccess)
			}
			report := fmt.Sprintf(`<nm_response><transaction><transaction_id>reported-1</transaction_id><original_transaction_id>%s</original_transaction_id><currency>USD</currency>%s<action><amount>%s</amount><action_type>refund</action_type><date>20261015090000</date><success>1</success></action></transaction></nm_response>`, tc.original, sale, tc.amount)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(report))
			}))
			defer server.Close()
			client, err := nmi.NewAccountClient(uuid.New(), uuid.New(), "fixture", &config.NMIProviderSettings{Environment: config.EnvironmentSandbox, SecurityKey: "fixture"})
			require.NoError(t, err)
			client.LoopbackFixture, client.ReadOnly, client.QueryURL = true, true, server.URL
			source := nmiSource{client: client, currency: "USD"}
			since := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
			rows, err := source.history(t.Context(), since, since.Add(24*time.Hour))
			require.NoError(t, err)
			refunds := 0
			for _, row := range rows {
				data := row.data.(transactionData)
				if data.Type != "refund" {
					continue
				}
				refunds++
				require.Equal(t, "reported-1", data.TransactionID)
				require.Equal(t, tc.wantOriginal, data.OriginalTransactionID)
				require.Equal(t, "reported-1:refund:2026-10-15T09:00:00Z", row.ref)
			}
			require.Equal(t, 1, refunds)
		})
	}
}
