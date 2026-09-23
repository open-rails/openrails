package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFetcher struct {
	provider Provider
	snap     *RemoteSnapshot
	err      error
}

func (f *fakeFetcher) Name() string { return string(f.provider) }
func (f *fakeFetcher) Capabilities() Capabilities {
	if f.snap != nil {
		return f.snap.Capabilities
	}
	return Capabilities{}
}
func (f *fakeFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snap, nil
}

func tp(t time.Time) *time.Time { return &t }

var testNow = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

func liveLocalSub(provider Provider, psid string) LocalSubscription {
	priceID := uuid.New()
	return LocalSubscription{
		ID:                    uuid.New(),
		CustomerID:            uuid.New(),
		PriceID:               &priceID,
		ProductID:             uuid.New(),
		Status:                "active",
		Rail:                  string(provider),
		RailSubscriptionID:    psid,
		CurrentPeriodStartsAt: tp(testNow.Add(-10 * 24 * time.Hour)),
		CurrentPeriodEndsAt:   tp(testNow.Add(20 * 24 * time.Hour)),
		StartedAt:             testNow.Add(-100 * 24 * time.Hour),
		EntitlementNames:      []string{"premium"},
	}
}

func nmiSaleTxn(txnID, orderID string, success bool) RemoteTransaction {
	t := RemoteTransaction{
		TransactionID: txnID,
		Type:          TransactionTypeSale,
		Success:       success,
		AmountCents:   999,
		Currency:      "USD",
		OccurredAt:    testNow.Add(-48 * time.Hour),
		Raw:           rawJSON(map[string]any{"order_id": orderID}),
	}
	if !success {
		t.Type = TransactionTypeDecline
		t.DeclineReason = "Insufficient funds"
	}
	return t
}

func findByType(findings []FindingRecord, t FindingType) []FindingRecord {
	var out []FindingRecord
	for _, f := range findings {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

func TestParseRebillOrderID(t *testing.T) {
	id := uuid.New()
	got, ok := parseRebillOrderID(fmt.Sprintf("rebill-%s-1765432100", id))
	require.True(t, ok)
	assert.Equal(t, id, got)

	got, ok = parseRebillOrderID(id.String())
	require.True(t, ok)
	assert.Equal(t, id, got)

	_, ok = parseRebillOrderID("rebill-not-a-uuid-123")
	assert.False(t, ok)
	_, ok = parseRebillOrderID("")
	assert.False(t, ok)
	_, ok = parseRebillOrderID("upgrade-12345678-87654321")
	assert.False(t, ok)
}
