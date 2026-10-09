//go:build e2e && integration

package ci_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A customer's prepaid money end to end through the admin API: create
// the customer, grant credit, admit and capture metered work, read the
// ledger, usage and profile, then revoke what remains.
func TestCustomerCreditsAdmissionsAndUsage(t *testing.T) {
	f := newFixture(t)
	client := f.runtime(t, "credits-"+uuid.NewString()[:8])
	ctx := t.Context()

	// Customer settings create a customer OpenRails has not seen.
	customer, other := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	for _, id := range []billing.CustomerID{customer, other} {
		_, err := client.UpdateCustomer(ctx, id, billing.UpdateCustomerParams{})
		require.NoError(t, err)
	}
	missing := billing.CustomerID(uuid.New())
	read, err := client.ListCustomers(ctx, billing.CustomerListParams{IDs: []billing.CustomerID{customer, missing, customer}})
	require.NoError(t, err)
	require.Len(t, read.Items, 1, "a named customer is answered once; an unseen one is absent")
	require.Nil(t, read.Items[0].Contact, "no directory holds a contact for it")

	found, err := client.ListCustomers(ctx, billing.CustomerListParams{Search: customer.String()})
	require.NoError(t, err)
	require.Len(t, found.Items, 1, "a search finds the customer whose id it is")
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

	grantParams := billing.CreateCreditGrantParams{Currency: "usd", Amount: 1_000_000, Source: "support", SourceID: "grant-1"}
	grant, err := createCreditGrant(ctx, client, customer, grantParams)
	require.NoError(t, err)
	require.False(t, grant.Replayed)
	require.Equal(t, billing.CreditGrantActive, grant.State)
	require.EqualValues(t, 1_000_000, grant.RemainingAmount)
	replay, err := createCreditGrant(ctx, client, customer, grantParams)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, grant.ID, replay.ID)
	grantParams.Amount = 2_000_000
	_, err = createCreditGrant(ctx, client, customer, grantParams)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	byKey, err := client.ListCreditGrants(ctx, customer, billing.CreditGrantListParams{SourceID: "grant-1"})
	require.NoError(t, err)
	require.Len(t, byKey.Items, 1)
	require.Equal(t, grant.ID, byKey.Items[0].ID)

	requestID := "job-" + uuid.NewString()
	deadline := time.Now().Add(time.Hour)
	verdicts, err := client.Admit(ctx, []billing.AdmitParams{{
		RequestID: requestID, CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer,
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
	capture := billing.CaptureAdmissionParams{Amount: 250_000, Usage: &billing.CaptureUsage{EventType: "inference"}}
	receipt, err := client.CaptureAdmission(ctx, requestID, capture)
	require.NoError(t, err)
	require.EqualValues(t, 250_000, receipt.Amount)
	require.NotNil(t, receipt.BalanceTransactionID)
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
	_, err = releaseAdmission(ctx, client, requestID)
	require.ErrorIs(t, err, billing.ErrConflict)

	event, err := recordUsage(ctx, client, billing.RecordUsageParams{
		CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference",
		Amount: 50_000, Source: "worker", SourceID: "event-1",
	})
	require.NoError(t, err)
	require.False(t, event.Replayed)
	_, err = recordUsage(ctx, client, billing.RecordUsageParams{
		CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "inference",
		Amount: 1, Source: "worker", SourceID: strings.Repeat("k", 256),
	})
	require.ErrorIs(t, err, billing.ErrInvalid, "a key longer than the table holds is invalid input")

	ledger, err := client.ListBalanceTransactions(ctx, customer, billing.BalanceTransactionListParams{Currency: "USD"})
	require.NoError(t, err)
	types := map[billing.BalanceTransactionType]int64{}
	for _, tx := range ledger.Items {
		types[tx.Type] += tx.Amount
	}
	require.Equal(t, map[billing.BalanceTransactionType]int64{billing.BalanceTransactionDeposit: 1_000_000, billing.BalanceTransactionSpend: -300_000}, types)

	usage, err := client.GetUsage(ctx, customer, billing.GetUsageParams{Currency: "USD", From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, billing.UsageByEventType, usage.GroupBy)
	require.Len(t, usage.Rows, 1)
	require.Equal(t, "inference", usage.Rows[0].Key)
	require.EqualValues(t, 2, usage.Rows[0].EventCount)
	require.EqualValues(t, 300_000, usage.Rows[0].Amount)

	_, err = client.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{TrustLevels: []billing.TrustLevel{{Currency: "USD", TrustLevel: "trusted"}}})
	require.NoError(t, err)
	settings, err := client.GetCustomer(ctx, customer)
	require.NoError(t, err)
	require.Equal(t, []billing.TrustLevel{{Currency: "USD", TrustLevel: "trusted"}}, settings.Settings.TrustLevels)

	revoked, err := client.RevokeCreditGrant(ctx, customer, grant.ID, billing.RevokeCreditGrantParams{Reason: "support correction"})
	require.NoError(t, err)
	require.Equal(t, billing.CreditGrantRevoked, revoked.State)
	require.EqualValues(t, 700_000, revoked.RevokedAmount)
	revokedAgain, err := client.RevokeCreditGrant(ctx, customer, grant.ID, billing.RevokeCreditGrantParams{Reason: "support correction"})
	require.NoError(t, err)
	require.True(t, revokedAgain.Replayed)

	profile, err := client.GetCustomer(ctx, customer)
	require.NoError(t, err)
	require.Equal(t, customer, profile.ID)
	require.Nil(t, profile.Contact)
	require.Len(t, profile.Balances, 1)
	require.EqualValues(t, 0, profile.Balances[0].BalanceAmount)

	// One merchant's customer is never another's.
	foreign := f.runtime(t, "credits-other-"+uuid.NewString()[:8])
	_, err = foreign.GetCreditGrant(ctx, customer, grant.ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = foreign.GetAdmission(ctx, requestID)
	require.ErrorIs(t, err, billing.ErrNotFound)
}
