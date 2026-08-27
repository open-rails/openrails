//go:build integration

package money_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
)

func TestProviderBillingObservationQualificationLifecycle(t *testing.T) {
	svc, dbi, _, payer, cur, ctx := moneyInEnvWithDB(t)
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	svc.SetClock(clock)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "th-045-evidence-" + uuid.NewString(),
	})
	require.NoError(t, err)

	open := func(label string) money.OperationAuthorizationInput {
		operationID := label + "-" + uuid.NewString()
		body := []byte(`{"operation_id":"` + operationID + `"}`)
		in := money.OperationAuthorizationInput{
			OperationID: operationID, Payer: payer, RecordOwner: "provider-controller",
			AuthorizedUSDMicros: 1_000, ClaimReference: "provider-operation:" + uuid.NewString(),
			AuthorizationBody: body, AuthorizationBodySHA256: sha256.Sum256(body),
		}
		tx, beginErr := dbi.Pool().Begin(ctx)
		require.NoError(t, beginErr)
		defer func() { _ = tx.Rollback(context.Background()) }()
		boundCtx, txDB, bindErr := dbi.BindMerchantTx(ctx, tx, dbtest.TestMerchantID)
		require.NoError(t, bindErr)
		_, openErr := svc.OpenOperationAuthorizationInTx(boundCtx, txDB, in, func(context.Context) (int64, error) { return 0, nil })
		require.NoError(t, openErr)
		require.NoError(t, tx.Commit(ctx))
		return in
	}
	record := func(in money.ProviderBillingObservationInput, commit bool) (*money.ProviderBillingQualification, error) {
		tx, beginErr := dbi.Pool().Begin(ctx)
		if beginErr != nil {
			return nil, beginErr
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		boundCtx, txDB, bindErr := dbi.BindMerchantTx(ctx, tx, dbtest.TestMerchantID)
		if bindErr != nil {
			return nil, bindErr
		}
		result, recordErr := svc.RecordProviderBillingObservationInTx(boundCtx, txDB, in, 24*time.Hour)
		if recordErr != nil {
			return nil, recordErr
		}
		if commit {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return nil, commitErr
			}
		}
		return result, nil
	}
	lifecycle := money.ProviderBillingLifecycleEvidence{
		Provider: "runpod", ProviderResourceID: "pod-1",
		ProviderLifetimeStart: now.Add(-2 * time.Hour), ProviderLifetimeEnd: now.Add(-time.Hour),
		ProviderAbsentAt: now.Add(-30 * time.Minute), ProviderAbsenceReference: "absence:pod-1",
		BillingStopReference: "billing-stop:pod-1", WindowsClosedAt: now.Add(-time.Hour),
		WindowsClosedReference: "windows-closed:pod-1",
		LifecycleEvidenceBody:  []byte(`{"provider_absent":true,"billing_stop":true,"open_windows":0}`),
	}
	mainAuth := open("th-045-zero")
	first := money.ProviderBillingObservationInput{
		OperationID: mainAuth.OperationID, ObservationID: "observation-1", Lifecycle: lifecycle,
		NormalizedQuery: "bucketSize=hour&grouping=podId&podId=pod-1", QueryStart: now.Add(-3 * time.Hour), QueryEnd: now,
		RawBody: []byte(`[]`), Records: []money.ProviderBillingRecord{},
	}
	firstResult, err := record(first, true)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationPending, firstResult.State)
	require.Equal(t, money.ProviderBillingAwaitingEqualObservation, firstResult.Reason)
	require.Equal(t, money.OperationAuthorizationOpen, firstResult.Authorization.State,
		"an empty provider response is zero-cost evidence, not finality")

	changedReplay := first
	changedReplay.RawBody = []byte("[ ]")
	_, err = record(changedReplay, true)
	require.ErrorIs(t, err, money.ErrProviderBillingObservationConflict)
	var conflict *money.ProviderBillingObservationConflict
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "raw_body", conflict.Field)

	clock.Advance(24 * time.Hour)
	replayed, err := record(first, true)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, money.ProviderBillingQualificationPending, replayed.State,
		"replaying one read after the lag must not impersonate a second provider observation")

	second := first
	second.ObservationID = "observation-2"
	rolledBack, err := record(second, false)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationEligible, rolledBack.State)
	stored, err := svc.GetProviderBillingQualification(ctx, mainAuth.OperationID)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationPending, stored.State)
	require.Equal(t, money.OperationAuthorizationOpen, stored.Authorization.State,
		"qualification and ledger settlement must roll back together")

	settled, err := record(second, true)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationEligible, settled.State)
	require.Equal(t, money.ProviderBillingEligible, settled.Reason)
	require.Equal(t, money.OperationAuthorizationSettled, settled.Authorization.State)
	require.NotNil(t, settled.QualifiedProviderCostUSDMicros)
	require.Zero(t, *settled.QualifiedProviderCostUSDMicros)

	refusedAuth := open("th-045-refused")
	refusedLifecycle := lifecycle
	refusedLifecycle.ProviderResourceID = "pod-refused"
	refusedLifecycle.ProviderAbsenceReference = "absence:pod-refused"
	refusedLifecycle.BillingStopReference = "billing-stop:pod-refused"
	refusedLifecycle.WindowsClosedReference = "windows-closed:pod-refused"
	refusedLifecycle.LifecycleEvidenceBody = []byte(`{"provider_absent":true,"resource":"pod-refused"}`)
	refusedInput := money.ProviderBillingObservationInput{
		OperationID: refusedAuth.OperationID, ObservationID: "refused-1", Lifecycle: refusedLifecycle,
		NormalizedQuery: "podId=pod-refused", QueryStart: now.Add(-3 * time.Hour), QueryEnd: now,
		RawBody: []byte(`[{"amount":0.0000001}]`),
		Refusal: &money.ProviderBillingObservationRefusal{Kind: "submicro_amount"},
	}
	refused, err := record(refusedInput, true)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationRefused, refused.State)
	require.Equal(t, money.ProviderBillingProviderEvidenceRefused, refused.Reason)
	require.Equal(t, money.OperationAuthorizationOpen, refused.Authorization.State)

	var raw []byte
	owner := dbtest.SharedSuperuserPGXPool(t)
	err = owner.QueryRow(ctx, `
		SELECT raw_body_bytes
		FROM openrails.provider_billing_observations
		WHERE merchant_id = $1 AND operation_id = $2 AND observation_id = $3`,
		dbtest.TestMerchantID.UUID(), refusedAuth.OperationID, refusedInput.ObservationID,
	).Scan(&raw)
	require.NoError(t, err)
	require.Equal(t, refusedInput.RawBody, raw, "typed SDK refusal retains exact bounded provider bytes")

	negativeAuth := open("th-045-negative")
	negativeLifecycle := refusedLifecycle
	negativeLifecycle.ProviderResourceID = "pod-negative"
	negativeLifecycle.ProviderAbsenceReference = "absence:pod-negative"
	negativeLifecycle.BillingStopReference = "billing-stop:pod-negative"
	negativeLifecycle.WindowsClosedReference = "windows-closed:pod-negative"
	negativeLifecycle.LifecycleEvidenceBody = []byte(`{"resource":"pod-negative"}`)
	negative, err := record(money.ProviderBillingObservationInput{
		OperationID: negativeAuth.OperationID, ObservationID: "negative-1", Lifecycle: negativeLifecycle,
		NormalizedQuery: "podId=pod-negative", QueryStart: now.Add(-3 * time.Hour), QueryEnd: now,
		RawBody: []byte(`[{"amount":-0.000001}]`), Records: []money.ProviderBillingRecord{{
			ProviderResourceID: "pod-negative", BucketStart: now.Add(-2 * time.Hour), AmountUSDMicros: -1, TimeBilledMS: 1,
		}},
	}, true)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationRefused, negative.State)
	require.Equal(t, money.ProviderBillingNegativeOrCorrective, negative.Reason)
	require.Equal(t, money.OperationAuthorizationOpen, negative.Authorization.State)

	decreasingAuth := open("th-045-decreasing")
	decreasingLifecycle := refusedLifecycle
	decreasingLifecycle.ProviderResourceID = "pod-decreasing"
	decreasingLifecycle.ProviderAbsenceReference = "absence:pod-decreasing"
	decreasingLifecycle.BillingStopReference = "billing-stop:pod-decreasing"
	decreasingLifecycle.WindowsClosedReference = "windows-closed:pod-decreasing"
	decreasingLifecycle.LifecycleEvidenceBody = []byte(`{"resource":"pod-decreasing"}`)
	decreasingFirst := money.ProviderBillingObservationInput{
		OperationID: decreasingAuth.OperationID, ObservationID: "decreasing-1", Lifecycle: decreasingLifecycle,
		NormalizedQuery: "podId=pod-decreasing", QueryStart: now.Add(-3 * time.Hour), QueryEnd: now,
		RawBody: []byte(`[{"amount":0.000010}]`), Records: []money.ProviderBillingRecord{{
			ProviderResourceID: "pod-decreasing", BucketStart: now.Add(-2 * time.Hour), AmountUSDMicros: 10, TimeBilledMS: 1,
		}},
	}
	baseline, err := record(decreasingFirst, true)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationPending, baseline.State)
	decreasingSecond := decreasingFirst
	decreasingSecond.ObservationID = "decreasing-2"
	decreasingSecond.RawBody = []byte(`[{"amount":0.000009}]`)
	decreasingSecond.Records = []money.ProviderBillingRecord{{
		ProviderResourceID: "pod-decreasing", BucketStart: now.Add(-2 * time.Hour), AmountUSDMicros: 9, TimeBilledMS: 1,
	}}
	decreasing, err := record(decreasingSecond, true)
	require.NoError(t, err)
	require.Equal(t, money.ProviderBillingQualificationRefused, decreasing.State)
	require.Equal(t, money.ProviderBillingDecreasingProviderCost, decreasing.Reason)
	require.Equal(t, money.OperationAuthorizationOpen, decreasing.Authorization.State)
}
