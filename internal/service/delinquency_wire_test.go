package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/delinquency"
)

// A delinquency travels with UTC times, its overdue amount as a decimal
// string (exact past 2^53) and the customer as a plain UUID.
func TestDelinquencyWire(t *testing.T) {
	customer := uuid.New()
	entered := time.Date(2026, 9, 16, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
	since := entered.Add(-time.Hour)
	raw, err := json.Marshal(delinquencyFromSnapshot(DelinquencySnapshot{
		CustomerID: customer, Currency: "USD", State: delinquency.StateDelinquent, OverdueStartedAt: &since,
		OverdueAmount: 9007199254740993, OverdueInvoices: 2, EnteredAt: entered, EvaluatedAt: entered,
	}))
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, customer.String(), wire["customer_id"])
	require.Equal(t, "9007199254740993", wire["overdue_amount"])
	require.Equal(t, "2026-09-16T11:00:00.123456789Z", wire["entered_at"])
	require.Equal(t, "2026-09-16T10:00:00.123456789Z", wire["overdue_started_at"])
}
