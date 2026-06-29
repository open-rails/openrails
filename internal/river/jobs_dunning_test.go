package riverjobs

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

func TestDunningWorkerSkipsPastDueWithoutPeriodEndWithoutPanic(t *testing.T) {
	worker := &DunningWorker{
		NMIClients: map[string]*nmi.NMIClient{
			string(models.RailNMI): {},
		},
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
