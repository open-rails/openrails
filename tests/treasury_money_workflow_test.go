//go:build integration

package tests

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

func checkTreasuryMoney(t *testing.T, f treasuryWorkflow) {
	ctx := merchant.WithID(t.Context(), f.merchant.MerchantID)
	ledger := money.NewMoneyService(dbtest.OpenMerchantDB(t, f.merchant.MerchantID.UUID()))
	fund := func(t *testing.T, amount int64) openrails.CustomerID {
		t.Helper()
		payer := openrails.CustomerID(uuid.New())
		_, err := f.client.EnsureCustomer(ctx, (payer).String())
		require.NoError(t, err)
		_, err = f.client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: new(payer.String()), Invoker: payer.String(), Currency: "USD", Amount: amount, Source: "treasury", SourceID: uuid.NewString()})
		require.NoError(t, err)
		return payer
	}
	admission := func(payer openrails.CustomerID, estimate int64) openrails.AdmitRequest {
		deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		return openrails.AdmitRequest{CustomerID: (payer).String(), Invoker: payer.String(), InvokerType: openrails.InvokerTypePayer,
			Currency: "USD", Source: "workflow", RequestID: uuid.NewString(), EstimatedAmount: estimate, ExpiresAt: &deadline}
	}
	admit := func(t *testing.T, req openrails.AdmitRequest) *openrails.AdmitResponse {
		t.Helper()
		result, err := f.client.Admit(ctx, req)
		require.NoError(t, err)
		require.True(t, result.Allowed)
		return result
	}
	assertLedger := func(t *testing.T, payer openrails.CustomerID, balance, held int64, withdrawals int) {
		t.Helper()
		account, err := f.client.Balance(ctx, (payer).String())
		require.NoError(t, err)
		require.Equal(t, payer.String(), account.CustomerID)
		require.Equal(t, "USD", account.Currency)
		require.Equal(t, "prepaid", account.BillingMode)
		require.Equal(t, balance, account.BalanceAmount)
		require.Equal(t, balance-held, account.AvailableAmount)
		require.Equal(t, held, account.HeldAmount)
		rows, total, err := ledger.GetTransactionsByCustomer(ctx, identity.CustomerID(payer), "USD", 200, 0)
		require.NoError(t, err)
		require.EqualValues(t, 1+withdrawals, total)
		require.Len(t, rows, 1+withdrawals, "holds are not ledger transfers")
		var sum int64
		counts := map[string]int{}
		for _, row := range rows {
			sum += row.Amount
			counts[row.TransactionType]++
		}
		require.Equal(t, balance, sum, "signed ledger amounts are conserved")
		require.Equal(t, 1, counts["deposit"])
		require.Equal(t, withdrawals, counts["withdrawal"])
	}
	for _, row := range []struct {
		name             string
		estimate, actual int64
	}{{"full", 3_000, 3_000}, {"partial", 4_000, 1_500}, {"metered", 5_000, 3_200}} {
		t.Run(row.name, func(t *testing.T) {
			payer := fund(t, 10_000)
			req := admission(payer, row.estimate)
			admit(t, req)
			assertLedger(t, payer, 10_000, row.estimate, 0)
			_, err := f.client.Capture(ctx, req.RequestID, row.actual, &openrails.CaptureUsage{EventType: "invoke", Resource: row.name})
			require.NoError(t, err)
			assertLedger(t, payer, 10_000-row.actual, 0, 1)
			probe := admission(payer, 10_000-row.actual)
			admit(t, probe)
			require.NoError(t, f.client.Release(ctx, probe.RequestID))
		})
	}
	t.Run("insufficient_balance", func(t *testing.T) {
		payer := fund(t, 1_000)
		req := admission(payer, 5_000)
		status, raw := requestWorkflowJSON(t, http.MethodPost, f.surface.BaseURL+"/v1/merchant/admissions", f.merchant.APIKey, map[string]any{"items": []openrails.AdmitRequest{req}})
		require.Equal(t, http.StatusOK, status, string(raw), "the batch envelope succeeds while this item is denied")
		items := workflowObject(t, raw)["items"].([]any)
		require.Len(t, items, 1)
		item := items[0].(map[string]any)
		require.EqualValues(t, http.StatusPaymentRequired, item["status"])
		result := item["result"].(map[string]any)
		require.Equal(t, false, result["allowed"])
		require.Equal(t, "money", result["blocked_by"])
		assertLedger(t, payer, 1_000, 0, 0)
	})
	t.Run("release", func(t *testing.T) {
		payer := fund(t, 8_000)
		req := admission(payer, 2_500)
		admit(t, req)
		require.NoError(t, f.client.Release(ctx, req.RequestID))
		assertLedger(t, payer, 8_000, 0, 0)
		probe := admission(payer, 8_000)
		admit(t, probe)
		require.NoError(t, f.client.Release(ctx, probe.RequestID))
	})
	t.Run("hold_replay", func(t *testing.T) {
		payer := fund(t, 10_000)
		req := admission(payer, 3_000)
		admit(t, req)
		require.True(t, admit(t, req).Replayed)
		probe := admission(payer, 7_000)
		admit(t, probe)
		blocked, err := f.client.AdmitBatch(ctx, []openrails.AdmitRequest{admission(payer, 1)})
		require.NoError(t, err)
		require.Equal(t, http.StatusPaymentRequired, blocked[0].Status)
		require.Equal(t, "money", blocked[0].Result.BlockedBy)
		require.NoError(t, f.client.Release(ctx, probe.RequestID))
		first, err := f.client.Capture(ctx, req.RequestID, 3_000, nil)
		require.NoError(t, err)
		replay, err := f.client.Capture(ctx, req.RequestID, 3_000, nil)
		require.NoError(t, err)
		first.Replayed = true
		require.Equal(t, first, replay, "amount-only retry needs no echoed admission metadata")
		_, err = f.client.Capture(ctx, req.RequestID, 3_001, nil)
		require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
		assertLedger(t, payer, 7_000, 0, 1)
	})
	t.Run("payer_isolation", func(t *testing.T) {
		a, b := fund(t, 5_000), fund(t, 9_000)
		reqA, reqB := admission(a, 2_000), admission(b, 3_000)
		admit(t, reqA)
		admit(t, reqB)
		_, err := f.client.Capture(ctx, reqA.RequestID, 2_000, nil)
		require.NoError(t, err)
		assertLedger(t, a, 3_000, 0, 1)
		assertLedger(t, b, 9_000, 3_000, 0)
		require.NoError(t, f.client.Release(ctx, reqB.RequestID))
		assertLedger(t, b, 9_000, 0, 0)
	})

	payer, other := fund(t, 10_000), fund(t, 3_000)
	for _, row := range []struct {
		payer openrails.CustomerID
		name  string
	}{{payer, "premium"}, {other, "foreign"}} {
		_, err := f.client.GrantEntitlement(ctx, (row.payer).String(), openrails.GrantEntitlementRequest{Entitlement: row.name})
		require.NoError(t, err)
	}
	batch, err := f.client.ListActiveEntitlements(ctx, []string{(payer).String(), (other).String()}, time.Now())
	require.NoError(t, err)
	for _, row := range []struct {
		payer openrails.CustomerID
		name  string
	}{{payer, "premium"}, {other, "foreign"}} {
		require.Len(t, batch[(row.payer).String()], 1)
		require.Equal(t, row.payer.String(), batch[(row.payer).String()][0].CustomerID)
		require.Equal(t, row.name, batch[(row.payer).String()][0].Entitlement)
	}
	serviceCaller := f.surface.RegisterServiceJWTIssuer("billing-service-"+uuid.NewString()[:8], f.merchant.MerchantSlug,
		[]string{controlplane.PermMerchantCustomerSettingsRead, controlplane.PermMerchantCustomerSettingsUpdate, controlplane.PermMerchantAdmissionsCreate})
	for _, token := range []string{f.merchant.APIKey, serviceCaller.Token} {
		status, raw := requestWorkflowJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/customers/"+payer.String()+"/entitlements", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var entitlements []map[string]any
		require.NoError(t, json.Unmarshal(raw, &entitlements))
		require.Len(t, entitlements, 1)
		require.Equal(t, payer.String(), entitlements[0]["customer_id"])
		require.Equal(t, "premium", entitlements[0]["entitlement"])
		require.Empty(t, entitlements[0]["user_id"])
		status, raw = requestWorkflowJSON(t, http.MethodGet, f.surface.BaseURL+"/v1/merchant/credits/balance?customer_id="+payer.String()+"&currency=USD", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		balance := workflowObject(t, raw)
		require.Equal(t, payer.String(), balance["customer_id"])
		require.Equal(t, "10000", balance["balance_amount"])
		require.Equal(t, "0", balance["held_amount"])
	}
}

func checkTreasuryDepositTerms(t *testing.T, f treasuryWorkflow) {
	for mode, client := range map[string]*openrails.Client{"remote": f.client, "embedded": f.embedded} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			payer := openrails.CustomerID(uuid.New())
			expires := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
			request := openrails.DepositCreditsRequest{CustomerID: new(payer.String()), Invoker: "original", Currency: "USD", Amount: 1_000_000,
				Source: "original", SourceID: uuid.NewString(), ExpiresAt: &expires, Description: "Original grant"}
			first, err := client.DepositCredits(ctx, request)
			require.NoError(t, err)
			require.False(t, first.Replayed)
			require.False(t, first.CreatedAt.IsZero())
			require.NotNil(t, first.Description)
			require.Equal(t, "Original grant", *first.Description)
			for _, field := range []string{"currency", "expiry", "amount"} {
				changed := request
				switch field {
				case "currency":
					changed.Currency = "EUR"
				case "expiry":
					changed.ExpiresAt = nil
				case "amount":
					changed.Amount++
				}
				_, err := client.DepositCredits(ctx, changed)
				require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused)
				var status *openrails.StatusError
				require.ErrorAs(t, err, &status)
				require.Equal(t, http.StatusConflict, status.Status)
			}
			request.Invoker, request.Source, request.Description = "replacement", "replacement", "Replacement grant"
			replay, err := client.DepositCredits(ctx, request)
			require.NoError(t, err)
			first.Replayed = true
			require.Equal(t, first, replay, "nonfinancial retry labels do not rewrite the accepted grant")
			read, err := client.GetDeposit(ctx, (payer).String(), request.SourceID)
			require.NoError(t, err)
			require.Equal(t, first, read)
			balance, err := client.Balance(ctx, (payer).String())
			require.NoError(t, err)
			require.EqualValues(t, 1_000_000, balance.BalanceAmount)
			missing, err := client.GetDeposit(ctx, (payer).String(), "never-"+uuid.NewString())
			require.ErrorIs(t, err, openrails.ErrNotFound)
			require.Nil(t, missing)
		})
	}
}
