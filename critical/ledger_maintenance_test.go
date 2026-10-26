//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestLedgerMaintenance(t *testing.T) {
	t.Run("OR-J31-S02/restore-ledger-is-validated-at-commit", func(t *testing.T) {
		for _, mode := range []string{"valid", "unfinished", "malformed", "changed-after-finish"} {
			t.Run(mode, func(t *testing.T) {
				source := newWorld(t)
				w, client := archiveTarget(t, source)
				merchant := client.MerchantID().UUID()
				tx, err := w.pool.Begin(t.Context())
				require.NoError(t, err)
				defer tx.Rollback(context.Background())
				_, err = tx.Exec(t.Context(), "SELECT set_config('openrails.merchant_id', $1, true)", merchant.String())
				require.NoError(t, err)
				_, err = tx.Exec(t.Context(), "SELECT "+pgx.Identifier{w.schema, "begin_billing_restore"}.Sanitize()+"($1)", merchant)
				require.NoError(t, err)
				// Deliberately malformed imported state: a counter has no transfer.
				// Assertions exercise the shipped database contract at COMMIT.
				corrupt := func() {
					_, err := tx.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "ledger_accounts"}.Sanitize()+
						" (merchant_id,account_type,currency,debits_must_not_exceed_credits,credits_must_not_exceed_debits,credits_posted) VALUES ($1,'processor_clearing','USD',false,false,1)", merchant)
					require.NoError(t, err)
				}
				if mode == "malformed" {
					corrupt()
				}
				if mode != "unfinished" {
					_, err = tx.Exec(t.Context(), "SELECT "+pgx.Identifier{w.schema, "finish_billing_restore"}.Sanitize()+"($1,$2,0)", merchant, strings.Repeat("a", 64))
					require.NoError(t, err, "receipt finalization leaves the ledger check to COMMIT")
				}
				if mode == "changed-after-finish" {
					corrupt()
				}
				err = tx.Commit(t.Context())
				switch mode {
				case "valid":
					require.NoError(t, err)
				case "unfinished":
					require.ErrorContains(t, err, "unfinished billing restore")
				default:
					require.ErrorContains(t, err, "billing restore ledger integrity mismatch")
				}
				if mode != "valid" {
					// The refused transaction leaves an empty, usable destination.
					_, err := client.ImportBillingArchive(t.Context(), bytes.NewReader(archiveBytes(t, source.alpha)))
					require.NoError(t, err)
				}
			})
		}
	})

	t.Run("OR-J22-S04/concurrent-credit-replay-and-manual-ledger-diagnostic", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		input := []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "concurrent-ledger"}}
		var results [2][]billing.CreditGrant
		var errs [2]error
		var group sync.WaitGroup
		start := make(chan struct{})
		for i := range results {
			group.Go(func() {
				<-start
				results[i], errs[i] = w.alpha.CreateCreditGrants(t.Context(), input)
			})
		}
		close(start)
		group.Wait()
		for i := range results {
			require.NoError(t, errs[i])
			require.Len(t, results[i], 1)
		}
		require.Equal(t, results[0][0].ID, results[1][0].ID)
		require.NotEqual(t, results[0][0].Replayed, results[1][0].Replayed)
		balance(t, w.alpha, customer, 1000, 0, 1000)
		ledger, err := w.alpha.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
		require.NoError(t, err)
		require.Len(t, ledger.Items, 1)
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")

		// Privileged corruption is fixture injection, not a supported money API.
		// The operator diagnostic must still detect it without a periodic worker.
		tx, err := w.pool.Begin(t.Context())
		require.NoError(t, err)
		defer tx.Rollback(context.Background())
		table := pgx.Identifier{w.schema, "ledger_accounts"}.Sanitize()
		_, err = tx.Exec(t.Context(), "ALTER TABLE "+table+" DISABLE TRIGGER guard_ledger_account_facts")
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "UPDATE "+table+" SET credits_posted=credits_posted+1 WHERE merchant_id=$1 AND customer_id=$2 AND account_type='customer_balance'", w.alpha.MerchantID().UUID(), customer.UUID())
		require.NoError(t, err)
		_, err = tx.Exec(t.Context(), "ALTER TABLE "+table+" ENABLE TRIGGER guard_ledger_account_facts")
		require.NoError(t, err)
		require.NoError(t, tx.Commit(t.Context()))
		output, err := hostCLIResult(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String())
		require.Error(t, err)
		require.Contains(t, output, "FAIL(1)")
		require.Contains(t, output, "conservation:")
		require.Contains(t, output, "counter drift:")
	})
}
