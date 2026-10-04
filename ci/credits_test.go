//go:build e2e && integration

package ci_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A customer's prepaid money end to end through the merchant API: declare
// the customer, grant credit, admit and capture metered work, read the
// ledger, usage and profile, then revoke what remains.
func TestCustomerCreditsAdmissionsAndUsage(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "credits-"+uuid.NewString()[:8])
	ctx := t.Context()

	customer := billing.CustomerID(uuid.New())
	email := "buyer@example.test"
	declared, err := client.EnsureCustomer(ctx, customer, billing.CustomerParams{Email: &email})
	require.NoError(t, err)
	require.Equal(t, &email, declared.Email)
	other := billing.CustomerID(uuid.New())
	_, err = client.EnsureCustomer(ctx, other, billing.CustomerParams{})
	require.NoError(t, err)
	_, err = client.GetCustomer(ctx, billing.CustomerID(uuid.New()))
	require.ErrorIs(t, err, billing.ErrNotFound)

	found, err := client.ListCustomers(ctx, billing.CustomerListParams{Query: "buyer@"})
	require.NoError(t, err)
	require.Len(t, found.Items, 1)
	require.Equal(t, customer, found.Items[0].ID)
	first, err := client.ListCustomers(ctx, billing.CustomerListParams{PageRequest: billing.PageRequest{Limit: 1}})
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.NotEmpty(t, first.Next)
	second, err := client.ListCustomers(ctx, billing.CustomerListParams{PageRequest: billing.PageRequest{Limit: 1, Cursor: first.Next}})
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	require.Empty(t, second.Next)
	require.ElementsMatch(t, []billing.CustomerID{customer, other}, []billing.CustomerID{first.Items[0].ID, second.Items[0].ID})

	grantParams := billing.CreditGrantParams{Currency: "usd", Amount: 1_000_000, Source: "support", SourceID: "grant-1"}
	grant, err := client.CreateCreditGrant(ctx, customer, grantParams)
	require.NoError(t, err)
	require.False(t, grant.Replayed)
	require.Equal(t, billing.CreditGrantActive, grant.State)
	require.EqualValues(t, 1_000_000, grant.RemainingAmount)
	replay, err := client.CreateCreditGrant(ctx, customer, grantParams)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, grant.ID, replay.ID)
	grantParams.Amount = 2_000_000
	_, err = client.CreateCreditGrant(ctx, customer, grantParams)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	byKey, err := client.ListCreditGrants(ctx, customer, billing.CreditGrantListParams{SourceID: "grant-1"})
	require.NoError(t, err)
	require.Len(t, byKey.Items, 1)
	require.Equal(t, grant.ID, byKey.Items[0].ID)

	requestID := "job-" + uuid.NewString()
	deadline := time.Now().Add(time.Hour)
	verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
		RequestID: requestID, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypePayer,
		Currency: "USD", EstimatedAmount: 300_000, ExpiresAt: &deadline,
	}})
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.True(t, verdicts[0].Allowed(), "%+v", verdicts[0])
	balance, err := client.GetBalance(ctx, customer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 1_000_000, balance.BalanceAmount)
	require.EqualValues(t, 300_000, balance.HeldAmount)
	require.EqualValues(t, 700_000, balance.AvailableAmount)

	open, err := client.GetAdmission(ctx, requestID)
	require.NoError(t, err)
	require.Equal(t, billing.AdmissionOpen, *open.State)
	capture := billing.CaptureParams{Amount: 250_000, Usage: &billing.CaptureUsage{EventType: "inference"}}
	receipt, err := client.CaptureAdmission(ctx, requestID, capture)
	require.NoError(t, err)
	require.EqualValues(t, 250_000, receipt.Amount)
	require.NotNil(t, receipt.CreditTransactionID)
	again, err := client.CaptureAdmission(ctx, requestID, capture)
	require.NoError(t, err)
	require.True(t, again.Replayed)
	capture.Amount = 260_000
	_, err = client.CaptureAdmission(ctx, requestID, capture)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	captured, err := client.GetAdmission(ctx, requestID)
	require.NoError(t, err)
	require.Equal(t, billing.AdmissionCaptured, *captured.State)
	require.EqualValues(t, 250_000, *captured.CapturedAmount)
	_, err = client.ReleaseAdmission(ctx, requestID)
	require.ErrorIs(t, err, billing.ErrConflict)

	event, err := client.RecordUsage(ctx, billing.UsageEventParams{
		CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference",
		Amount: 50_000, Source: "worker", SourceID: "event-1",
	})
	require.NoError(t, err)
	require.False(t, event.Replayed)

	ledger, err := client.ListCreditTransactions(ctx, customer, billing.CreditTransactionListParams{Currency: "USD"})
	require.NoError(t, err)
	types := map[billing.CreditTransactionType]int64{}
	for _, tx := range ledger.Items {
		types[tx.Type] += tx.Amount
	}
	require.Equal(t, map[billing.CreditTransactionType]int64{billing.CreditDeposit: 1_000_000, billing.CreditSpend: -300_000}, types)

	usage, err := client.GetUsage(ctx, customer, billing.UsageParams{Currency: "USD", From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, billing.UsageByEventType, usage.GroupBy)
	require.Len(t, usage.Rows, 1)
	require.Equal(t, "inference", usage.Rows[0].Key)
	require.EqualValues(t, 2, usage.Rows[0].EventCount)
	require.EqualValues(t, 300_000, usage.Rows[0].Amount)

	_, err = client.SetTrustLevel(ctx, customer, billing.TrustLevelParams{Currency: "USD", TrustLevel: "trusted"})
	require.NoError(t, err)
	trust, err := client.GetTrustLevel(ctx, customer, "USD")
	require.NoError(t, err)
	require.Equal(t, "trusted", trust.TrustLevel)

	revoked, err := client.RevokeCreditGrant(ctx, customer, grant.ID, billing.RevokeCreditGrantParams{Reason: "support correction"})
	require.NoError(t, err)
	require.Equal(t, billing.CreditGrantRevoked, revoked.State)
	require.EqualValues(t, 700_000, revoked.RevokedAmount)
	revokedAgain, err := client.RevokeCreditGrant(ctx, customer, grant.ID, billing.RevokeCreditGrantParams{Reason: "support correction"})
	require.NoError(t, err)
	require.True(t, revokedAgain.Replayed)

	profile, err := client.GetCustomerBillingProfile(ctx, customer)
	require.NoError(t, err)
	require.Equal(t, customer, profile.Customer.ID)
	require.Equal(t, &email, profile.Customer.Email)
	require.Len(t, profile.Balances, 1)
	require.EqualValues(t, 0, profile.Balances[0].BalanceAmount)

	// One merchant's customer is never another's.
	foreign := f.runtime(t, "credits-other-"+uuid.NewString()[:8])
	_, err = foreign.GetCreditGrant(ctx, customer, grant.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = foreign.GetAdmission(ctx, requestID)
	require.ErrorIs(t, err, billing.ErrNotFound)
}
