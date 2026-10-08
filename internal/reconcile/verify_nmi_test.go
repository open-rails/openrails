package reconcile

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// A batched transaction report names no schedule: sales are attributed by
// vault, then order; a vault two rows share sends both to single reads.
func TestAttribution(t *testing.T) {
	sub := func(rail string) *models.Subscription {
		return &models.Subscription{ID: uuid.New(), RailSubscriptionID: rail}
	}
	a, b, c, d := sub("s-a"), sub("s-b"), sub("s-c"), sub("s-d")
	vaults := map[uuid.UUID]string{a.ID: "v1", b.ID: "v2", c.ID: "v2"}
	records := map[string]nmi.ScheduleRecord{"s-c": {OrderID: "order-c"}}
	at := newAttribution([]*models.Subscription{a, b, c, d}, vaults, records)
	sale := func(vault, order, schedule string) nmi.ScheduleSale {
		return nmi.ScheduleSale{SaleAction: nmi.SaleAction{TransactionID: uuid.NewString(), Success: true, Amount: "9.99", Currency: "USD", At: time.Now()}, VaultID: vault, OrderID: order, SubscriptionID: schedule}
	}
	require.NoError(t, at.add(sale("v1", "", "")))
	require.NoError(t, at.add(sale("v2", "order-c", "")))
	require.NoError(t, at.add(sale("v9", "", "s-b")))
	require.NoError(t, at.add(sale("v2", "", "")))
	require.Len(t, at.txns[a.ID], 1, "a vault only one row holds")
	require.Len(t, at.txns[c.ID], 1, "a shared vault resolved by the schedule's order")
	require.Len(t, at.txns[b.ID], 1, "a report that names the schedule")
	require.Equal(t, "s-a", at.txns[a.ID][0].SubscriptionID)
	require.True(t, at.ambiguous[b.ID] && at.ambiguous[c.ID], "an unattributable sale on a shared vault")
	require.True(t, at.ambiguous[d.ID], "a row with no known vault is read alone")
	require.False(t, at.ambiguous[a.ID])

	solo := newAttribution([]*models.Subscription{d}, nil, nil)
	require.NoError(t, solo.add(sale("v7", "", "")))
	require.Len(t, solo.txns[d.ID], 1, "a single-schedule query answers only for it")
}
