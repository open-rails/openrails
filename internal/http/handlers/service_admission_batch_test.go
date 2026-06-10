package handlers

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// TestServiceAdmitBatchVerdicts_MixedVerdictsAndIsolation proves the #335 batch
// admission contract: per-item verdicts with single-admit semantics, and FULL
// per-item isolation — one item's bad input, scope denial, money deny, or
// backend error never fails the other items.
func TestServiceAdmitBatchVerdicts_MixedVerdictsAndIsolation(t *testing.T) {
	allowedPayer := uuid.NewString()
	brokePayer := uuid.NewString()
	throttledPayer := uuid.NewString()
	erroringPayer := uuid.NewString()
	scopedOutPayer := uuid.NewString()

	items := []serviceAdmitRequest{
		{TenantSubjectID: allowedPayer, Actor: "user:a", CreditType: "usd_micro", EstimateMicros: 100, RequestID: "r1"},
		{TenantSubjectID: brokePayer, Actor: "user:b", CreditType: "usd_micro", EstimateMicros: 100, RequestID: "r2"},
		{TenantSubjectID: "not-a-uuid", Actor: "user:c", RequestID: "r3"},
		{TenantSubjectID: throttledPayer, Actor: "user:d", RequestID: "r4"},
		{TenantSubjectID: erroringPayer, Actor: "user:e", RequestID: "r5"},
		{TenantSubjectID: scopedOutPayer, Actor: "user:f", RequestID: "r6"},
		{TenantSubjectID: allowedPayer, Actor: "user:g", EstimateMicros: -1, RequestID: "r7"},
	}

	allows := func(ts billingidentity.TenantSubjectID) bool {
		return ts.UUID().String() != scopedOutPayer
	}
	admit := func(_ context.Context, in billingservice.AdmitInput) (*billingservice.AdmitResult, error) {
		switch in.TenantSubjectID.UUID().String() {
		case allowedPayer:
			return &billingservice.AdmitResult{Allowed: true, ReservationID: uuid.NewString()}, nil
		case brokePayer:
			return &billingservice.AdmitResult{Allowed: false, BlockedBy: "money", DenyCode: "insufficient_balance"}, nil
		case throttledPayer:
			return &billingservice.AdmitResult{Allowed: false, BlockedBy: "throughput", RetryAfterSeconds: 7}, nil
		default:
			return nil, fmt.Errorf("backend exploded")
		}
	}

	out := serviceAdmitBatchVerdicts(context.Background(), items, allows, admit)
	require.Len(t, out, len(items))

	// Item 0: allowed.
	require.Equal(t, http.StatusOK, out[0].Status)
	require.NotNil(t, out[0].Result)
	require.True(t, out[0].Result.Allowed)

	// Item 1: money deny -> 402 with the deny code, batch unharmed.
	require.Equal(t, http.StatusPaymentRequired, out[1].Status)
	require.NotNil(t, out[1].Result)
	require.Equal(t, "insufficient_balance", out[1].Result.DenyCode)

	// Item 2: malformed payer id -> per-item 400.
	require.Equal(t, http.StatusBadRequest, out[2].Status)
	require.Nil(t, out[2].Result)

	// Item 3: throughput deny -> 429.
	require.Equal(t, http.StatusTooManyRequests, out[3].Status)
	require.NotNil(t, out[3].Result)
	require.Equal(t, int64(7), out[3].Result.RetryAfterSeconds)

	// Item 4: backend error -> per-item 500, NOT a batch failure.
	require.Equal(t, http.StatusInternalServerError, out[4].Status)
	require.Equal(t, "admission check failed", out[4].Error)

	// Item 5: token tenant-subject scope denied -> per-item 403.
	require.Equal(t, http.StatusForbidden, out[5].Status)
	require.Equal(t, "service_token_tenant_subject_scope_denied", out[5].Error)

	// Item 6: negative estimate -> per-item 400.
	require.Equal(t, http.StatusBadRequest, out[6].Status)
}
