//go:build integration

package riverjobs

import (
	"context"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestDunningWorker_RebillSuccessWithoutBundledCredits(t *testing.T) {
	f := newDunningCertaintyFixture(t, 720, time.Minute, true)
	require.Equal(t, dunningOutcomeSucceeded, f.run(t))
	require.NoError(t, f.dbi.RunInMerchantConn(f.ctx, func(ctx context.Context) error {
		sub, err := f.subSvc.GetByID(ctx, f.subID)
		if err != nil {
			return err
		}
		balance, err := f.moneySvc.GetBalanceForCustomer(ctx, identity.CustomerID(sub.CustomerID), "USD")
		if err != nil {
			return err
		}
		require.Zero(t, balance.Balance, "subscription renewal never creates an optional bundled balance")
		return nil
	}))
}
