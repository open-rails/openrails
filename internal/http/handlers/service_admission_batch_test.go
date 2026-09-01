package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
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
	abuseLimitedPayer := uuid.NewString()
	erroringPayer := uuid.NewString()
	scopedOutPayer := uuid.NewString()

	items := []serviceAdmitRequest{
		{CustomerID: allowedPayer, Invoker: "user:a", EstimatedAmount: 100, ExpiresAt: holdDeadline(), RequestID: "r1"},
		{CustomerID: brokePayer, Invoker: "user:b", EstimatedAmount: 100, ExpiresAt: holdDeadline(), RequestID: "r2"},
		{CustomerID: "not-a-uuid", Invoker: "user:c", RequestID: "r3"},
		{CustomerID: abuseLimitedPayer, Invoker: "user:d", RequestID: "r4"},
		{CustomerID: erroringPayer, Invoker: "user:e", RequestID: "r5"},
		{CustomerID: scopedOutPayer, Invoker: "user:f", RequestID: "r6"},
		{CustomerID: allowedPayer, Invoker: "user:g", EstimatedAmount: -1, RequestID: "r7"},
	}

	allows := func(ts billingidentity.CustomerID) bool {
		return ts.UUID().String() != scopedOutPayer
	}
	admit := func(_ context.Context, in billingservice.AdmitInput) (*billingservice.AdmitResult, error) {
		switch in.CustomerID.UUID().String() {
		case allowedPayer:
			return &billingservice.AdmitResult{Allowed: true}, nil
		case brokePayer:
			return &billingservice.AdmitResult{Allowed: false, BlockedBy: "money", DenyCode: "insufficient_balance"}, nil
		case abuseLimitedPayer:
			return &billingservice.AdmitResult{Allowed: false, BlockedBy: "abuse", RetryAfterSeconds: 7}, nil
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

	// Item 3: abuse deny -> 429.
	require.Equal(t, http.StatusTooManyRequests, out[3].Status)
	require.NotNil(t, out[3].Result)
	require.Equal(t, int64(7), out[3].Result.RetryAfterSeconds)

	// Item 4: backend error -> per-item 500, NOT a batch failure.
	require.Equal(t, http.StatusInternalServerError, out[4].Status)
	require.Equal(t, "admission check failed", out[4].Error)

	// Item 5: token merchant-subject scope denied -> per-item 403.
	require.Equal(t, http.StatusForbidden, out[5].Status)
	require.Equal(t, "service_credential_customer_scope_denied", out[5].Error)

	// Item 6: negative estimate -> per-item 400.
	require.Equal(t, http.StatusBadRequest, out[6].Status)
}

func TestAdmitInputFromRequest_UsesTrustLevel(t *testing.T) {
	payer := billingidentity.CustomerID(uuid.New())

	got := admitInputFromRequest(serviceAdmitRequest{TrustLevel: "trusted"}, payer)
	require.Equal(t, "trusted", got.TrustLevel)
}

// TestServiceAdmitBatchVerdicts_LogsTheCause is th#1627's regression gate. The
// verdict's wire string is a deliberate constant — it crosses into the host's
// tenant-facing error — but the CAUSE must reach the operator log. It did not:
// `403 credit_authorize_failed ... status=500 admission check failed` was the
// ONLY signal a billed-invoke outage produced, so attributing it cost a bisect
// across two standing stacks instead of one grep.
func TestServiceAdmitBatchVerdicts_LogsTheCause(t *testing.T) {
	cause := fmt.Errorf(`ERROR: relation "openrails.billing_policy_bindings" does not exist (SQLSTATE 42P01)`)

	var buf bytes.Buffer
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.ErrorLevel)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetLevel(prevLevel) })

	payer := uuid.NewString()
	out := serviceAdmitBatchVerdicts(context.Background(),
		[]serviceAdmitRequest{{CustomerID: payer, Invoker: "user:a", RequestID: "r1", Source: "tensorhub"}},
		func(billingidentity.CustomerID) bool { return true },
		func(context.Context, billingservice.AdmitInput) (*billingservice.AdmitResult, error) {
			return nil, cause
		},
	)

	require.Equal(t, http.StatusInternalServerError, out[0].Status)
	require.Equal(t, "admission check failed", out[0].Error, "the wire string stays stable and non-leaky")

	logged := buf.String()
	require.Contains(t, logged, "admission check failed")
	// logrus escapes quotes in the text formatter, so match the payload, not the framing.
	require.Contains(t, logged, "billing_policy_bindings", "the cause must reach the operator log")
	require.Contains(t, logged, "42P01", "including the SQLSTATE that names the failure class")
	require.Contains(t, logged, payer, "and be attributable to a payer")
}

// holdDeadline is the declared deadline every hold-placing admit must carry
// (xs-007 row 33): an hour from now, as a job would declare.
func holdDeadline() *int64 {
	v := time.Now().Add(time.Hour).Unix()
	return &v
}
