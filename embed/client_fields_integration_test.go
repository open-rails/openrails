//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// A real policy decision and persisted grant expose fields lost between the
// public request and the engine. Both clients use the same operation script.
func TestClientAdmissionFieldsAndDelegationProvenance(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	host := h.StartEmbeddedHost("USD")
	standalone := h.StartStandalone("USD")
	local, localErr := host.Runtime().Client(openrails.WithCurrency("USD"))
	if localErr != nil {
		t.Fatal(localErr)
	}
	require.NoError(t, local.SetMerchantSettings(ctx, openrails.MerchantSettings{
		BillingPolicies: []openrails.BillingPolicyInput{{
			Name: "client_rate", Kind: "accrual_rate_cap", AccrualRateCapPerHour: 10_000_000,
		}},
		BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "client_rate"}},
	}))

	for name, client := range map[string]*openrails.Client{"embedded": local, "remote": standalone.Client()} {
		t.Run(name, func(t *testing.T) {
			payer := uuid.New()
			_, err := h.Pool().Exec(ctx, `INSERT INTO openrails.customers
				(id, merchant_id, issuer, created_at, last_seen_at)
				VALUES ($1, $2, 'client-fields', now(), now())`, payer, dbtest.TestMerchantID.UUID())
			require.NoError(t, err)
			id := openrails.CustomerID(payer)
			_, err = client.DepositCredits(ctx, openrails.DepositCreditsRequest{
				CustomerID: &id, Invoker: "client-fields", Currency: "USD", Amount: 1_000_000,
				Source: "client-fields", SourceID: uuid.NewString(),
			})
			require.NoError(t, err)
			expires := time.Now().Add(time.Hour).Unix()
			req := openrails.AdmitRequest{
				CustomerID: payer.String(), Invoker: "client-fields", InvokerType: "payer", Currency: "USD",
				EstimatedAmount: 1000, ExpiresAt: &expires, AccrualRateDeltaPerHour: 11_000_000,
				RequestID: uuid.NewString(), Source: "client-fields", Resource: "compute", TrustLevel: "standard",
			}
			verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{req})
			require.NoError(t, err)
			require.Len(t, verdicts, 1)
			require.NotNil(t, verdicts[0].Result)
			require.False(t, verdicts[0].Result.Allowed)
			require.Equal(t, "accrual_rate_cap_reached", verdicts[0].Result.DenyCode)
			{
				single := client
				verdict, err := single.Admit(ctx, req)
				require.NoError(t, err)
				require.False(t, verdict.Allowed)
				require.Equal(t, "accrual_rate_cap_reached", verdict.DenyCode)
			}
			req.AccrualRateDeltaPerHour = 1_000_000
			verdicts, err = client.AdmitBatch(ctx, []openrails.AdmitRequest{req})
			require.NoError(t, err)
			require.True(t, verdicts[0].Result.Allowed)
			require.NoError(t, client.Release(ctx, req.RequestID))

			grant := openrails.SpendDelegationInput{
				Scope: "invoker", ScopeKey: "client-fields", Provenance: "original-policy",
				Windows: []openrails.SpendLimitWindow{{Key: "5h", WindowSeconds: 18000, Limit: 1_000_000, Currency: "USD"}},
			}
			readProvenance := func() string {
				t.Helper()
				var value string
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT provenance FROM openrails.invoker_spend_limits
					WHERE merchant_id=$1 AND customer_id=$2 AND scope=$3 AND scope_key=$4`,
					dbtest.TestMerchantID.UUID(), payer, grant.Scope, grant.ScopeKey).Scan(&value))
				return value
			}
			require.NoError(t, client.SetCustomerSpendDelegations(ctx, payer.String(), []openrails.SpendDelegationInput{grant}))
			require.Equal(t, grant.Provenance, readProvenance())
			grant.Provenance = "updated-policy"
			require.NoError(t, client.SetCustomerSpendDelegation(ctx, payer.String(), grant))
			require.Equal(t, grant.Provenance, readProvenance())
		})
	}
}
