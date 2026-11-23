package reconcile

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

func TestSolanaPlanDiscoveryOnBulkRead(t *testing.T) {
	first := solanago.PublicKey{1}.String()
	second := solanago.PublicKey{2}.String()
	scanned := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		var opts struct {
			Filters []struct {
				Memcmp struct {
					Offset uint64 `json:"offset"`
					Bytes  string `json:"bytes"`
				} `json:"memcmp"`
			} `json:"filters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			req.Method != "getProgramAccounts" || len(req.Params) != 2 ||
			json.Unmarshal(req.Params[1], &opts) != nil || len(opts.Filters) != 2 ||
			opts.Filters[1].Memcmp.Offset != subscriptions.SubscriptionAccountDelegateeOffset {
			t.Error("expected a Solana subscription account scan scoped to one plan")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		scanned <- opts.Filters[1].Memcmp.Bytes
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(struct {
			Protocol string          `json:"jsonrpc"`
			ID       json.RawMessage `json:"id"`
			Result   []any           `json:"result"`
		}{Protocol: "2.0", ID: req.ID, Result: []any{}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	rpc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{PublicURL: server.URL, ReadOnly: true})
	fetcher := NewSolanaFetcher(slog.Default(), rpc, func(context.Context) ([]SolanaSubscriptionRef, error) { return nil, nil })
	fetcher.Plans = func(context.Context) ([]string, error) { return []string{first, second, first}, nil }

	morning := time.Date(2026, time.November, 19, 0, 0, 0, 0, time.UTC)
	for _, now := range []time.Time{morning, morning.Add(12 * time.Hour)} {
		_, err := fetcher.Fetch(t.Context(), FetchParams{ObservedAt: now})
		require.NoError(t, err)
		require.Len(t, scanned, 2)
		require.ElementsMatch(t, []string{first, second}, []string{<-scanned, <-scanned})
	}

	_, err := fetcher.Fetch(t.Context(), FetchParams{ObservedAt: morning, SkipSolanaPlanDiscovery: true})
	require.NoError(t, err)
	require.Empty(t, scanned)

	_, err = fetcher.Fetch(t.Context(), FetchParams{ObservedAt: morning, SubscriptionID: first})
	require.NoError(t, err)
	require.Empty(t, scanned)
}
