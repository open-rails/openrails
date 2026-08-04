package delinquency

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
)

// The floor default is DERIVED from the collection floor, and the two must not
// drift: the whole argument for the derivation is that they answer the same
// question ("how small is too small to chase"). Pinned here rather than by
// importing the constant into production code, so the money package stays out
// of this module's dependency set.
func TestDefaultAmountFloorMatchesTheCollectionFloor(t *testing.T) {
	require.Equal(t, money.DefaultInvoiceMonthlyFloorAmount, DefaultAmountFloor,
		"the delinquency floor default must equal the invoice monthly floor default — "+
			"a debt too small to bother COLLECTING is too small to cut anyone off for")
}

func TestClassify(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	policy := Policy{GraceDays: 7, AmountFloor: 1_000_000}

	tests := []struct {
		name     string
		exposure Exposure
		want     State
	}{
		{
			name:     "nothing overdue is current",
			exposure: Exposure{},
			want:     StateCurrent,
		},
		{
			name: "a zero-amount overdue invoice is current, not grace",
			exposure: Exposure{
				OverdueSince: now.AddDate(0, 0, -30), OverdueAmount: 0, OverdueInvoices: 1,
			},
			want: StateCurrent,
		},
		{
			name: "overdue inside the grace window is grace",
			exposure: Exposure{
				OverdueSince: now.AddDate(0, 0, -3), OverdueAmount: 5_000_000, OverdueInvoices: 1,
			},
			want: StateGrace,
		},
		{
			name: "the grace boundary itself is still grace",
			exposure: Exposure{
				OverdueSince: now.AddDate(0, 0, -7).Add(time.Second), OverdueAmount: 5_000_000, OverdueInvoices: 1,
			},
			want: StateGrace,
		},
		{
			name: "past grace and over the floor is delinquent",
			exposure: Exposure{
				OverdueSince: now.AddDate(0, 0, -8), OverdueAmount: 5_000_000, OverdueInvoices: 1,
			},
			want: StateDelinquent,
		},
		{
			name: "below the floor never escalates, however old",
			exposure: Exposure{
				OverdueSince: now.AddDate(0, 0, -400), OverdueAmount: 999_999, OverdueInvoices: 3,
			},
			want: StateGrace,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Classify(policy, tc.exposure, now))
		})
	}
}

// Grace 0 is a valid explicit merchant choice: delinquent the moment it is
// overdue. It must not be read as "unset" and silently replaced by the default.
func TestClassifyZeroGraceIsRespected(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	policy := Policy{GraceDays: 0, AmountFloor: 0}
	require.Equal(t, StateDelinquent, Classify(policy, Exposure{
		OverdueSince: now.Add(-time.Minute), OverdueAmount: 1, OverdueInvoices: 1,
	}, now))
}

func TestPolicyFromConfig(t *testing.T) {
	grace := 30
	floor := int64(25_000_000)
	monthly := int64(3_000_000)
	zero := 0

	t.Run("defaults when the merchant declared nothing", func(t *testing.T) {
		p, err := PolicyFromConfig(models.MerchantConfiguration{})
		require.NoError(t, err)
		require.Equal(t, Policy{GraceDays: DefaultGraceDays, AmountFloor: DefaultAmountFloor}, p)
	})

	t.Run("the floor is derived from the invoice monthly floor", func(t *testing.T) {
		p, err := PolicyFromConfig(models.MerchantConfiguration{InvoiceMonthlyFloor: &monthly})
		require.NoError(t, err)
		require.Equal(t, monthly, p.AmountFloor,
			"a merchant that already declared what is too small to collect has answered this question")
	})

	t.Run("an explicit delinquency floor wins over the derivation", func(t *testing.T) {
		p, err := PolicyFromConfig(models.MerchantConfiguration{
			InvoiceMonthlyFloor: &monthly, ArrearsDelinquencyFloor: &floor,
		})
		require.NoError(t, err)
		require.Equal(t, floor, p.AmountFloor)
	})

	t.Run("declared grace wins over the default", func(t *testing.T) {
		p, err := PolicyFromConfig(models.MerchantConfiguration{ArrearsGraceDays: &grace})
		require.NoError(t, err)
		require.Equal(t, 30, p.GraceDays)
	})

	t.Run("zero grace is an explicit choice, not an unset value", func(t *testing.T) {
		p, err := PolicyFromConfig(models.MerchantConfiguration{ArrearsGraceDays: &zero})
		require.NoError(t, err)
		require.Equal(t, 0, p.GraceDays)
	})

	t.Run("negative values are a config error, not a silent clamp", func(t *testing.T) {
		neg := -1
		_, err := PolicyFromConfig(models.MerchantConfiguration{ArrearsGraceDays: &neg})
		require.Error(t, err)
	})
}

// An unreadable state must never be read as "cut this customer off".
func TestParseStateFailsSafe(t *testing.T) {
	require.Equal(t, StateCurrent, ParseState(""))
	require.Equal(t, StateCurrent, ParseState("garbage"))
	require.Equal(t, StateDelinquent, ParseState("delinquent"))
	require.Equal(t, StateGrace, ParseState(" GRACE "))
}
