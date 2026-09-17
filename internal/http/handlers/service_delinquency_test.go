package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/delinquency"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// The delinquency roster spells instants at full RFC3339 precision, money as
// a decimal string and the customer as its plain UUID.
func TestServiceDelinquencyRowsWire(t *testing.T) {
	customer := uuid.New()
	entered := time.Date(2026, 9, 16, 12, 0, 0, 123456789, time.UTC)
	since := entered.Add(-time.Hour)
	rows := serviceDelinquencyRows([]billingservice.DelinquencySnapshot{{
		CustomerID: customer, Currency: "USD", State: delinquency.StateDelinquent, OverdueSince: &since,
		OverdueAmount: 9007199254740993, OverdueInvoices: 2, EnteredAt: entered, EvaluatedAt: entered,
	}})
	raw, err := json.Marshal(rows)
	require.NoError(t, err)
	var wire []map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, customer.String(), wire[0]["customer_id"])
	require.Equal(t, "9007199254740993", wire[0]["overdue_amount"])
	require.Equal(t, "2026-09-16T12:00:00.123456789Z", wire[0]["entered_at"])
	require.Equal(t, "2026-09-16T11:00:00.123456789Z", wire[0]["overdue_since"])
}
