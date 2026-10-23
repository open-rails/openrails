package admission

import (
	"math"
	"testing"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestUsagePreemptiveCollectionRequiresUsefulCapacity(t *testing.T) {
	for _, c := range []struct {
		balance, held, limit, cost int64
		want                       bool
	}{
		{0, 0, 100, 5, true}, {0, 0, 100, 101, false},
		{10, 0, 100, 105, true}, {0, 20, 100, 90, false},
		{0, 20, 100, 80, true}, {math.MaxInt64, 0, 100, math.MaxInt64, true},
	} {
		capacity := money.AdmissionCapacity{Balance: c.balance, Held: c.held, CreditLimit: c.limit, HardCreditLimit: true}
		require.Equal(t, c.want, canFitAfterDebtSettlement(capacity, ResolvedPolicy{}, c.cost))
	}
	require.False(t, canFitAfterDebtSettlement(money.AdmissionCapacity{CreditLimit: 100, HardCreditLimit: true}, ResolvedPolicy{OutstandingCapAmount: 50}, 60))
}
