package reconcile

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// executionWindow is the loader's live-lease contract; other loader methods
// are not involved in deciding whether a charge is still being recorded.
type executionWindow struct {
	LocalStateLoader
	operation uuid.UUID
	expires   time.Time
}

func (l executionWindow) ExecutingOperations(_ context.Context, _ uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if !at.Before(l.expires) {
		return out, nil
	}
	if len(ids) == 0 {
		out[uuid.Nil] = true
		return out, nil
	}
	for _, id := range ids {
		if id == l.operation {
			out[id] = true
		}
	}
	return out, nil
}

type operationChargeReader struct {
	RailFetcher
	read func() uuid.UUID
}

func (r operationChargeReader) OperationOf(context.Context, *RemoteTransaction) (uuid.UUID, error) {
	return r.read(), nil
}

// Looking up a provider's metadata can outlive the lease observed before the
// read. Such a charge is still missing and must retain its financial finding.
func TestChargeLeaseIsCurrentAfterProviderRead(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	operation := uuid.New()
	e := &Engine{Local: executionWindow{operation: operation, expires: now.Add(time.Second)}, Now: func() time.Time { return now }}
	reader := operationChargeReader{read: func() uuid.UUID { now = now.Add(2 * time.Second); return operation }}
	finding := Finding{Type: FindingChargeMissingLocal, SubjectKey: "charge-after-expiry"}
	snapshot := &RemoteSnapshot{Transactions: []RemoteTransaction{{TransactionID: finding.SubjectKey, Type: TransactionTypeSale, Success: true}}}
	got, _ := e.deferOperationCharges(t.Context(), ProviderStripe, uuid.New(), reader, snapshot, []Finding{finding})
	require.Equal(t, []Finding{finding}, got, "a lease that expired during the metadata read owns no charge now")
}

// Provider descriptions that were not written in the renewal format must
// not borrow an active operation's lease to suppress a missing-charge finding.
func TestNMIChargeOperationRequiresRenewalDescription(t *testing.T) {
	operation := uuid.MustParse("0199d634-a534-780b-861f-8b9577eb5f8b")
	for _, tc := range []struct {
		name, description string
		want              uuid.UUID
	}{
		{"renewal", "OpenRails renewal " + operation.String(), operation},
		{"provider description", operation.String(), uuid.Nil},
		{"noncanonical renewal", "OpenRails renewal " + strings.ToUpper(operation.String()), uuid.Nil},
		{"zero operation", "OpenRails renewal " + uuid.Nil.String(), uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]string{"order_description": tc.description})
			require.NoError(t, err)
			id, err := (&NMIFetcher{}).OperationOf(t.Context(), &RemoteTransaction{Raw: raw})
			require.NoError(t, err)
			require.Equal(t, tc.want, id)
		})
	}
}
