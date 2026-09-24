package delinquency

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
)

func TestClassify(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	policy := Policy{GraceDays: 7, AmountFloor: 1_000_000}
	overdue := func(age time.Duration, amount int64, invoices int) Exposure {
		return Exposure{OverdueSince: now.Add(-age), OverdueAmount: amount, OverdueInvoices: invoices}
	}
	cases := []struct {
		name   string
		policy Policy
		e      Exposure
		want   State
	}{
		{"nothing overdue", policy, Exposure{}, StateCurrent},
		{"zero-amount overdue invoice", policy, overdue(30*24*time.Hour, 0, 1), StateCurrent},
		{"amount without invoices", policy, overdue(30*24*time.Hour, 5_000_000, 0), StateCurrent},
		{"inside grace", policy, overdue(3*24*time.Hour, 5_000_000, 1), StateGrace},
		{"one second before grace ends", policy, overdue(7*24*time.Hour-time.Second, 5_000_000, 1), StateGrace},
		{"grace boundary is delinquent", policy, overdue(7*24*time.Hour, 5_000_000, 1), StateDelinquent},
		{"exactly the floor escalates", policy, overdue(8*24*time.Hour, 1_000_000, 1), StateDelinquent},
		{"below the floor never escalates", policy, overdue(400*24*time.Hour, 999_999, 3), StateGrace},
		// Zero grace is an explicit choice, not "unset".
		{"zero grace", Policy{}, overdue(time.Minute, 1, 1), StateDelinquent},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, Classify(tc.policy, tc.e, now), tc.name)
	}
}

func TestPolicyFromConfig(t *testing.T) {
	// A debt too small to bother collecting is too small to cut anyone off for.
	require.Equal(t, money.DefaultInvoiceMonthlyFloorAmount, DefaultAmountFloor)

	grace, zero, neg := 30, 0, -1
	floor, monthly, negFloor := int64(25_000_000), int64(3_000_000), int64(-1)
	defaults := Policy{GraceDays: DefaultGraceDays, AmountFloor: DefaultAmountFloor}
	cases := []struct {
		name    string
		cfg     models.MerchantConfiguration
		want    Policy
		wantErr bool
	}{
		{"defaults", models.MerchantConfiguration{}, defaults, false},
		{"floor derived from invoice monthly floor", models.MerchantConfiguration{InvoiceMonthlyFloor: &monthly},
			Policy{GraceDays: DefaultGraceDays, AmountFloor: monthly}, false},
		{"explicit floor wins", models.MerchantConfiguration{InvoiceMonthlyFloor: &monthly, ArrearsDelinquencyFloor: &floor},
			Policy{GraceDays: DefaultGraceDays, AmountFloor: floor}, false},
		{"declared grace", models.MerchantConfiguration{ArrearsGraceDays: &grace}, Policy{GraceDays: 30, AmountFloor: DefaultAmountFloor}, false},
		{"zero grace kept", models.MerchantConfiguration{ArrearsGraceDays: &zero}, Policy{AmountFloor: DefaultAmountFloor}, false},
		{"negative grace rejected", models.MerchantConfiguration{ArrearsGraceDays: &neg}, Policy{}, true},
		{"negative floor rejected", models.MerchantConfiguration{ArrearsDelinquencyFloor: &negFloor}, Policy{}, true},
	}
	for _, tc := range cases {
		got, err := PolicyFromConfig(tc.cfg)
		if tc.wantErr {
			require.Error(t, err, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}

// -1 is the no-override sentinel; 0 is a real override.
func TestPolicyOverrides(t *testing.T) {
	base := Policy{GraceDays: 14, AmountFloor: 1_000_000}
	got, err := base.withOverrides(-1, -1)
	require.NoError(t, err)
	require.Equal(t, base, got)
	got, err = base.withOverrides(0, 0)
	require.NoError(t, err)
	require.Equal(t, Policy{}, got)
	got, err = base.withOverrides(3, -1)
	require.NoError(t, err)
	require.Equal(t, Policy{GraceDays: 3, AmountFloor: 1_000_000}, got)
	_, err = Policy{GraceDays: -2}.withOverrides(-1, -1)
	require.Error(t, err)
}

// An unreadable stored state must never read as "cut this customer off".
func TestParseStateFailsSafe(t *testing.T) {
	for in, want := range map[string]State{
		"": StateCurrent, "garbage": StateCurrent, "delinquent": StateDelinquent,
		" GRACE ": StateGrace, "Current": StateCurrent,
	} {
		require.Equal(t, want, ParseState(in), "%q", in)
	}
}
