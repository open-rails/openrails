//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/solanafake"
)

func TestHumanAmountSolanaPayRecordsNativeDenomination(t *testing.T) {
	for _, tc := range []struct {
		name, token, amount, mint string
		native                    int64
	}{
		{"SOL", "SOL", "1 SOL", solanafake.DevnetSOLMint, 1_000_000_000},
		{"USDC", "USDC", "10 USDC", solanafake.DevnetUSDCMint, 10_000_000},
		{"PYUSD", "PYUSD", "2.5 PYUSD", solanafake.DevnetPYUSDMint, 2_500_000},
		{"SOL-above-2pow53", "SOL", "9007199.254740993 SOL", solanafake.DevnetSOLMint, 9_007_199_254_740_993},
	} {
		for _, tp := range []topology{embedded, remote} {
			t.Run(tc.name+"/"+string(tp), func(t *testing.T) {
				w := prepareWorld(t, 30)
				fake, _ := withSolana(t, w)
				declare := w.declare
				w.declare = func(psps map[string]openrails.PSPConfig) {
					declare(psps)
					tokens := psps["solana"].Settings["tokens"].(map[string]any)
					tokens["USDC"] = map[string]any{}
					tokens["PYUSD"] = map[string]any{}
				}
				w.start()
				chain := &solanaPay{w: w, fake: fake, stopWorkers: map[*world]func(){}}
				chain.runWorkers(w)
				productKey, err := w.applyCatalog(fmt.Sprintf(`  "{key}-native":
    amount: %s
    psps: [solana]
`, tc.amount))
				require.NoError(t, err)
				price, err := w.client[tp].GetPriceByKey(t.Context(), productKey, productKey+"-native")
				require.NoError(t, err)
				require.Equal(t, tc.token, price.Currency)
				require.Equal(t, tc.native, price.UnitAmount)
				buyer := w.newCustomer()
				params := billing.CreateCheckoutAttemptParams{
					Customer: buyer.identity(), PriceID: price.ID, Entitlement: productKey,
					IdempotencyKey: "native-token-" + uuid.NewString(),
					PaymentOptions: billing.CheckoutPaymentOptions{PSP: "solana", TokenSymbol: tc.token, Flow: "transfer_request"},
					SuccessURL:     "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
				}
				attempt, err := w.client[tp].CreateCheckoutAttempt(t.Context(), params)
				require.NoError(t, err)
				require.Equal(t, billing.CheckoutAttemptRequiresAction, attempt.Status)
				require.NotNil(t, attempt.NextAction)
				require.Equal(t, "solana_pay", attempt.NextAction.Type)
				transfer := chain.sessionTerms(buyer, attempt)
				require.Equal(t, uint64(tc.native), transfer.amount, "same-token price is already in atomic chain units")
				require.Equal(t, tc.mint, transfer.mint)
				signature := chain.pay(transfer, transfer.amount)
				for range 2 {
					confirmed, err := w.client[tp].ConfirmCheckoutAttempt(t.Context(), attempt.ID, billing.ConfirmCheckoutAttemptParams{Signature: signature})
					require.NoError(t, err)
					require.Equal(t, billing.CheckoutAttemptSucceeded, confirmed.Status)
				}
				replayed, err := w.client[tp].CreateCheckoutAttempt(t.Context(), params)
				require.NoError(t, err)
				require.Equal(t, attempt.ID, replayed.ID)
				require.Equal(t, billing.CheckoutAttemptSucceeded, replayed.Status)
				w.settle()
				paid := completed(w.payments(tp, buyer.id))
				require.Len(t, paid, 1, "a chain signature and accepted checkout settle once")
				require.Equal(t, tc.token, paid[0].Currency)
				require.Equal(t, tc.native, paid[0].Amount, "the stored payment retains its native denomination without USD conversion")
				require.Equal(t, price.ID, paid[0].PriceID)
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
}
