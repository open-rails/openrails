//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/solanafake"
)

func TestHumanAmountSolanaPayRecordsNativeDenomination(t *testing.T) {
	for _, tc := range []struct {
		name, token, amount, mint string
		native                    int64
	}{
		{"SOL", "SOL", "1 SOL", solanafake.DevnetSOLMint, 1_000_000_000},
		{"SOL-above-2pow53", "SOL", "9007199.254740993 SOL", solanafake.DevnetSOLMint, 9_007_199_254_740_993},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := prepareWorld(t, 30)
			fake := withSolana(t, w).fake
			w.start()
			chain := &solanaPay{w: w, fake: fake, stopWorkers: map[*world]func(){}}
			chain.runWorkers(w)
			productKey, err := w.applyCatalog(fmt.Sprintf(`  "{key}-native":
    amount: %s
    psps: {solana: {}}
`, tc.amount))
			require.NoError(t, err)
			price, err := priceByKey(t.Context(), w.client[embedded], productKey, productKey+"-native")
			require.NoError(t, err)
			require.Equal(t, tc.token, price.Currency)
			require.Equal(t, tc.native, price.UnitAmount)
			chain.prices = map[string]string{tc.token: price.ID.String()}
			chain.rail = w.options(price.ID)["solana"]
			buyer := w.newCustomer()
			transfer := chain.purchaseIn(buyer, tc.token)
			require.Equal(t, uint64(tc.native), transfer.amount, "same-token price is already in atomic chain units")
			require.Equal(t, tc.mint, transfer.mint)
			signature := chain.pay(transfer, transfer.amount)
			chain.until(func() bool { return chain.status(transfer) == "succeeded" }, "the poller credits the transfer")
			for range 2 {
				require.Equal(t, "complete", chain.confirm(transfer), "a repeated confirm answers the paid order")
			}
			w.settle()
			paid := completed(w.payments(embedded, buyer.id))
			require.Len(t, paid, 1, "a chain signature and an order settle once")
			require.Equal(t, tc.token, paid[0].Currency)
			require.Equal(t, tc.native, paid[0].Amount, "the stored payment retains its native denomination without USD conversion")
			require.Equal(t, transfer.order, *paid[0].OrderID)
			var raw []byte
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT metadata FROM billing.payments WHERE id=$1`), paid[0].ID.UUID()).Scan(&raw))
			var metadata map[string]any
			require.NoError(t, json.Unmarshal(raw, &metadata))
			require.Equal(t, strconv.FormatInt(tc.native, 10), metadata["solana_token_amount"], "native amounts stay exact across generic JSON readers")
			require.Equal(t, strconv.FormatInt(tc.native, 10), metadata["solana_received"])

			require.Equal(t, 1, chain.payments(transfer))
			require.True(t, buyer.entitled(productKey))
			disposition, reason := chain.receipt(signature)
			require.Equal(t, "credited", disposition)
			require.Empty(t, reason)
		})
	}
}
