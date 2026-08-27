//go:build integration

package money_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	owner := dbtest.SharedSuperuserPGXPool(t)
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
	_, err = svc.ReleaseOperationAuthorization(ctx, mainAuth.OperationID, "provider-create-refused")
	require.ErrorIs(t, err, money.ErrOperationAuthorizationHasBillingEvidence,
		"durable billing evidence makes release an invalid terminal path")
	pendingReplay, err := record(first, true)
	require.NoError(t, err)
	require.True(t, pendingReplay.Replayed)
	require.Equal(t, money.OperationAuthorizationOpen, pendingReplay.Authorization.State,
		"exact observation replay cannot report an authorization released around its qualification")

	_, err = owner.Exec(ctx, `
		UPDATE openrails.provider_billing_qualifications
		SET provider_lifetime_end = provider_lifetime_start
		WHERE merchant_id = $1 AND operation_id = $2`,
		dbtest.TestMerchantID.UUID(), mainAuth.OperationID,
	)
	require.Error(t, err, "Postgres must refuse a zero-length provider-confirmed lifetime")

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

	settleTx, err := owner.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = settleTx.Rollback(context.Background()) }()
	_, err = settleTx.Exec(ctx, "LOCK TABLE openrails.operation_authorizations IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)
	settleCtx, settleDB, err := dbi.BindMerchantTx(ctx, settleTx, dbtest.TestMerchantID)
	require.NoError(t, err)
	settled, err := svc.RecordProviderBillingObservationInTx(settleCtx, settleDB, second, 24*time.Hour)
	require.NoError(t, err)
	type qualificationRead struct {
		result *money.ProviderBillingQualification
		err    error
	}
	readDone := make(chan qualificationRead, 1)
	go func() {
		result, readErr := svc.GetProviderBillingQualification(ctx, mainAuth.OperationID)
		readDone <- qualificationRead{result: result, err: readErr}
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := owner.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks
				WHERE relation = 'openrails.operation_authorizations'::regclass
				  AND mode = 'AccessShareLock'
				  AND NOT granted
			)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "qualification read must reach the authorization table lock")
	require.NoError(t, settleTx.Commit(ctx))
	snapshot := <-readDone
	require.NoError(t, snapshot.err)
	before := snapshot.result.State == money.ProviderBillingQualificationPending &&
		snapshot.result.Authorization.State == money.OperationAuthorizationOpen
	after := snapshot.result.State == money.ProviderBillingQualificationEligible &&
		snapshot.result.Authorization.State == money.OperationAuthorizationSettled
	require.True(t, before || after,
		"one joined read must return a pre-commit or post-commit pair, got qualification=%s authorization=%s",
		snapshot.result.State, snapshot.result.Authorization.State)
	require.Equal(t, money.ProviderBillingQualificationEligible, settled.State)
	require.Equal(t, money.ProviderBillingEligible, settled.Reason)
	require.Equal(t, money.OperationAuthorizationSettled, settled.Authorization.State)
	require.NotNil(t, settled.QualifiedProviderCostUSDMicros)
	require.Zero(t, *settled.QualifiedProviderCostUSDMicros)
	require.Contains(t, string(settled.Authorization.SettlementBody), `"observation_id":"observation-1"`)
	require.Contains(t, string(settled.Authorization.SettlementBody), `"observation_id":"observation-2"`)
	rawDigest := sha256.Sum256(first.RawBody)
	require.Contains(t, string(settled.Authorization.SettlementBody), hex.EncodeToString(rawDigest[:]),
		"terminal settlement evidence must bind the exact qualified provider body")

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
	_, err = svc.ReleaseOperationAuthorization(ctx, refusedAuth.OperationID, "provider-create-refused")
	require.ErrorIs(t, err, money.ErrOperationAuthorizationHasBillingEvidence,
		"refused evidence remains durable and cannot be erased through authorization release")

	var raw []byte
	err = owner.QueryRow(ctx, `
		SELECT raw_body_bytes
		FROM openrails.provider_billing_observations
		WHERE merchant_id = $1 AND operation_id = $2 AND observation_id = $3`,
		dbtest.TestMerchantID.UUID(), refusedAuth.OperationID, refusedInput.ObservationID,
	).Scan(&raw)
	require.NoError(t, err)
	require.Equal(t, refusedInput.RawBody, raw, "typed SDK refusal retains exact bounded provider bytes")

	_, err = owner.Exec(ctx, `
		UPDATE openrails.provider_billing_observations
		SET raw_body_available = false,
		    raw_body_bytes = ''::bytea,
		    raw_body_digest = public.digest(''::bytea, 'sha256')
		WHERE merchant_id = $1 AND operation_id = $2 AND observation_id = $3`,
		dbtest.TestMerchantID.UUID(), refusedAuth.OperationID, refusedInput.ObservationID,
	)
	require.Error(t, err, "Postgres must refuse discarding exact bytes from a bounded parser refusal")
	_, err = owner.Exec(ctx, `
		UPDATE openrails.provider_billing_observations
		SET refusal_kind = 'response_too_large'
		WHERE merchant_id = $1 AND operation_id = $2 AND observation_id = $3`,
		dbtest.TestMerchantID.UUID(), refusedAuth.OperationID, refusedInput.ObservationID,
	)
	require.Error(t, err, "Postgres must refuse retaining partial bytes from an oversized response")

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
