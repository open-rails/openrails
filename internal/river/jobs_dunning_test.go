package riverjobs

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

// fakeDunningNMIResolver is a static money.NMIClientResolver stand-in for the
// #725 store-armed builder.
type fakeDunningNMIResolver struct{ client *nmi.NMIClient }

func (f fakeDunningNMIResolver) ResolveNMIClient(_ context.Context, _ uuid.UUID, _ *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if f.client == nil {
		return nil, false, nil
	}
	return f.client, true, nil
}

func TestDunningWorkerSkipsPastDueWithoutPeriodEndWithoutPanic(t *testing.T) {
	worker := &DunningWorker{
		NMIResolver: fakeDunningNMIResolver{client: &nmi.NMIClient{}},
	}
	sub := &models.Subscription{
		ID:                 uuid.New(),
		Status:             models.StatusPastDue,
		Rail:               models.RailNMI,
		RailSubscriptionID: "sub_missing_period",
	}

	require.NotPanics(t, func() {
		outcome := worker.processSubscription(context.Background(), sub, nil, nil, nil, false)
		require.Equal(t, dunningOutcomeFailed, outcome)
	})
}
